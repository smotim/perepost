package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

type channelKey struct {
	platform platform
	id       int64
}

func (c channelKey) String() string {
	return string(c.platform) + ":" + strconv.FormatInt(c.id, 10)
}

type store struct {
	*sql.DB
	lock *os.File
}

func openStore(ctx context.Context, path string) (*store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()

		return nil, fmt.Errorf("база уже используется другим процессом: %w", err)
	}
	// SQLite creates its side files using the permissions of the database.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = lock.Close()

		return nil, err
	}
	_ = file.Close()
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		_ = lock.Close()

		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &store{db, lock}
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS inbox (
			source TEXT NOT NULL,
			mid TEXT NOT NULL,
			post TEXT NOT NULL,
			payload BLOB,
			received INTEGER NOT NULL,
			UNIQUE (source, mid)
		);
		CREATE INDEX IF NOT EXISTS inbox_pending ON inbox(source) WHERE payload IS NOT NULL;
		CREATE INDEX IF NOT EXISTS inbox_post ON inbox(source, post) WHERE payload IS NOT NULL;
		CREATE TABLE IF NOT EXISTS deliveries (
			source TEXT NOT NULL,
			post TEXT NOT NULL,
			destination TEXT NOT NULL,
			state TEXT NOT NULL,
			PRIMARY KEY (source, post, destination)
		);
		CREATE INDEX IF NOT EXISTS deliveries_blocked ON deliveries(destination)
			WHERE state IN ('sending', 'uncertain');
		CREATE TABLE IF NOT EXISTS published (
			channel TEXT NOT NULL,
			mid TEXT NOT NULL,
			PRIMARY KEY (channel, mid)
		);
		CREATE TABLE IF NOT EXISTS mirrors (
			source TEXT NOT NULL,
			post TEXT NOT NULL,
			destination TEXT NOT NULL,
			mid TEXT NOT NULL,
			kind TEXT NOT NULL,
			UNIQUE (destination, mid)
		);
		CREATE INDEX IF NOT EXISTS mirror_post ON mirrors(source, post, destination);
		CREATE TABLE IF NOT EXISTS album_parts (
			source TEXT NOT NULL,
			post TEXT NOT NULL,
			mid TEXT NOT NULL,
			payload BLOB NOT NULL,
			PRIMARY KEY (source, post, mid)
		);
		CREATE TABLE IF NOT EXISTS revisions (
			source TEXT NOT NULL,
			mid TEXT NOT NULL,
			stamp INTEGER NOT NULL,
			seq INTEGER NOT NULL,
			PRIMARY KEY (source, mid)
		);
		UPDATE deliveries SET state = 'uncertain' WHERE state = 'sending';
		UPDATE deliveries SET state = 'failed' WHERE state = 'partial';
	`)
	if err != nil {
		s.close()

		return nil, err
	}

	return s, nil
}

func (s *store) close() {
	_ = s.Close()
	_ = s.lock.Close()
}

func (s *store) enqueue(ctx context.Context, source channelKey, mid, post string, payload []byte, received time.Time) error {
	_, err := s.ExecContext(ctx, `
		INSERT INTO inbox(source, mid, post, payload, received) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (source, mid) DO NOTHING
	`, source.String(), mid, post, payload, received.UnixMilli())

	return err
}

type pendingPost struct {
	key      string
	row      int64 // last included row, so an arrival during delivery stays in the inbox
	payloads [][]byte
}

func (s *store) next(ctx context.Context, source channelKey, now time.Time) (pendingPost, error) {
	var p pendingPost
	err := s.QueryRowContext(ctx, `
		SELECT post FROM inbox
		WHERE source = ? AND payload IS NOT NULL AND NOT EXISTS (
			SELECT 1 FROM deliveries WHERE destination = inbox.source AND state IN ('sending', 'uncertain')
		) ORDER BY rowid LIMIT 1
	`, source.String()).Scan(&p.key)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	rows, err := s.QueryContext(ctx, `
		SELECT rowid, payload, received, EXISTS (
			SELECT 1 FROM published WHERE channel = inbox.source AND mid = inbox.mid
		) FROM inbox
		WHERE source = ? AND post = ? AND payload IS NOT NULL ORDER BY rowid
	`, source.String(), p.key)
	if err != nil {
		return p, err
	}
	defer func() { _ = rows.Close() }()
	var received int64
	for rows.Next() {
		var payload []byte
		var echo bool
		if err := rows.Scan(&p.row, &payload, &received, &echo); err != nil {
			return p, err
		}
		if !echo {
			p.payloads = append(p.payloads, payload)
		}
	}
	if err := rows.Err(); err != nil {
		return p, err
	}
	if source.platform == telegramPlatform && strings.HasPrefix(p.key, "album:") && now.Sub(time.UnixMilli(received)) < albumWait {
		return pendingPost{}, nil
	}

	return p, nil
}

func (s *store) complete(ctx context.Context, source channelKey, p pendingPost) error {
	_, err := s.ExecContext(ctx, `UPDATE inbox SET payload = NULL WHERE source = ? AND post = ? AND rowid <= ?`,
		source.String(), p.key, p.row)

	return err
}

type deliveryBook struct {
	store  *store
	source channelKey
	post   string
	root   string // a revision can publish extra parts for an existing post
	err    error  // a storage failure must stop processing before another publish
}

type publish func(context.Context, func() ([]string, error), ...mirrorKind) error

func (b *deliveryBook) deliver(ctx context.Context, dst channelKey, send func(publish) error) error {
	if b == nil {
		return send(func(_ context.Context, fn func() ([]string, error), _ ...mirrorKind) error {
			_, err := fn()

			return err
		})
	}
	if b.err != nil {
		return b.err
	}
	var state string
	err := b.store.QueryRowContext(ctx, `
		SELECT state FROM deliveries WHERE source = ? AND post = ? AND destination = ?
	`, b.source.String(), b.post, dst.String()).Scan(&state)
	if err == nil {
		return nil // a prior process already attempted this destination
	}
	if !errors.Is(err, sql.ErrNoRows) {
		b.err = err

		return err
	}

	sendErr := send(func(ctx context.Context, fn func() ([]string, error), kinds ...mirrorKind) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if b.err = b.set(ctx, dst, "sending", nil); b.err != nil {
			return b.err
		}
		ids, err := fn()
		state = "partial"
		switch {
		case err == nil && len(ids) == 0:
			err = errors.New("платформа не вернула id опубликованных сообщений")
			state = "uncertain"
		case errors.Is(err, errRejected):
			state = "failed"
		case err != nil:
			state = "uncertain"
		}
		saving, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if b.err = b.set(saving, dst, state, ids, kinds...); b.err != nil {
			return b.err
		}
		if state == "uncertain" {
			return fmt.Errorf("результат публикации неизвестен, source=%s post=%s destination=%s: %w", b.source, b.post, dst, err)
		}

		return err
	})
	if b.err != nil || state == "uncertain" || state == "failed" || (state == "" && ctx.Err() != nil) {
		return sendErr
	}
	state = "done"
	if sendErr != nil {
		state = "failed"
	}
	saving, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if b.err = b.set(saving, dst, state, nil); b.err != nil {
		return b.err
	}

	return sendErr
}

func (s *store) unresolved(ctx context.Context) error {
	rows, err := s.QueryContext(ctx, `SELECT source, post, destination FROM deliveries WHERE state = 'uncertain'`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var source, post, destination string
		if err := rows.Scan(&source, &post, &destination); err != nil {
			return err
		}
		slog.Warn("результат публикации неизвестен, обратная пересылка приостановлена",
			"source", source, "post", post, "destination", destination)
	}

	return rows.Err()
}

func (s *store) resolveUncertain(ctx context.Context, source, post, destination string, ids []string) error {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE deliveries SET state = 'failed'
		WHERE source = ? AND post = ? AND destination = ? AND state = 'uncertain'
	`, source, post, destination)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return errors.New("публикация со статусом uncertain не найдена")
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO published(channel, mid) VALUES (?, ?) ON CONFLICT DO NOTHING`, destination, id); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (b *deliveryBook) set(ctx context.Context, dst channelKey, state string, ids []string, kinds ...mirrorKind) error {
	tx, err := b.store.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for i, id := range ids {
		if id == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO published(channel, mid) VALUES (?, ?) ON CONFLICT DO NOTHING`, dst.String(), id); err != nil {
			return err
		}
		if i < len(kinds) {
			root := b.root
			if root == "" {
				root = b.post
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mirrors(source, post, destination, mid, kind) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (destination, mid) DO NOTHING
			`, b.source.String(), root, dst.String(), id, kinds[i]); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO deliveries(source, post, destination, state) VALUES (?, ?, ?, ?)
		ON CONFLICT (source, post, destination) DO UPDATE SET state = excluded.state
	`, b.source.String(), b.post, dst.String(), state); err != nil {
		return err
	}

	return tx.Commit()
}
