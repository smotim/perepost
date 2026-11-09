package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type change struct {
	Kind     string
	Mid      string
	Group    string
	At       int64
	Sequence int64
	Message  json.RawMessage
}

type mirrorKind string

const (
	mirrorText    mirrorKind = "text"
	mirrorCaption mirrorKind = "caption"
	mirrorMedia   mirrorKind = "media"
)

type mirrorPart struct {
	id   string
	kind mirrorKind
}

func (s *store) enqueueChange(ctx context.Context, source channelKey, event change) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("change:%x", sha256.Sum256(payload))

	return s.enqueue(ctx, source, key, key, payload, time.Now())
}

func (s *store) acceptRevision(ctx context.Context, source channelKey, event change) (bool, error) {
	result, err := s.ExecContext(ctx, `
		INSERT INTO revisions(source, mid, stamp, seq) VALUES (?, ?, ?, ?)
		ON CONFLICT(source, mid) DO UPDATE SET stamp = excluded.stamp, seq = excluded.seq
		WHERE excluded.stamp > stamp OR (excluded.stamp = stamp AND excluded.seq >= seq)
	`, source.String(), event.Mid, event.At, event.Sequence)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()

	return n == 1, err
}

func (b *deliveryBook) mirrors(ctx context.Context, destination channelKey) ([]mirrorPart, error) {
	rows, err := b.store.QueryContext(ctx, `
		SELECT mid, kind FROM mirrors WHERE source = ? AND post = ? AND destination = ? ORDER BY rowid
	`, b.source.String(), b.root, destination.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var parts []mirrorPart
	for rows.Next() {
		var part mirrorPart
		if err := rows.Scan(&part.id, &part.kind); err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}

	return parts, rows.Err()
}

func (b *deliveryBook) forgetMirror(ctx context.Context, dst channelKey, part mirrorPart) error {
	_, err := b.store.ExecContext(ctx, `DELETE FROM mirrors WHERE destination = ? AND mid = ?`, dst.String(), part.id)

	return err
}

func (s *store) keepAlbum(ctx context.Context, source channelKey, post string, payloads [][]byte) error {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, payload := range payloads {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO album_parts(source, post, mid, payload) VALUES (?, ?, json_extract(?, '$.message_id'), ?)
			ON CONFLICT DO NOTHING
		`, source.String(), post, string(payload), payload); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// splitLateAlbum gives new parts their own delivery identities once any
// destination has attempted the original album. Existing parts stay frozen so
// a resumed partial delivery keeps the same publication order.
func (s *store) splitLateAlbum(ctx context.Context, source channelKey, p pendingPost) (bool, error) {
	result, err := s.ExecContext(ctx, `
		UPDATE inbox SET post = post || '#' || mid
		WHERE source = ? AND post = ? AND rowid <= ? AND payload IS NOT NULL
		AND EXISTS (SELECT 1 FROM deliveries WHERE source = inbox.source AND post = inbox.post)
		AND NOT EXISTS (SELECT 1 FROM album_parts WHERE source = inbox.source AND post = inbox.post AND mid = inbox.mid)
	`, source.String(), p.key, p.row)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()

	return n > 0, err
}

func (s *store) albumPayloads(ctx context.Context, source channelKey, post string) ([][]byte, error) {
	rows, err := s.QueryContext(ctx, `SELECT payload FROM album_parts WHERE source = ? AND post = ? ORDER BY CAST(mid AS INTEGER)`,
		source.String(), post)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var payloads [][]byte
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}

	return payloads, rows.Err()
}

func (s *store) reviseAlbum(ctx context.Context, source channelKey, event change) (string, [][]byte, error) {
	var root string
	err := s.QueryRowContext(ctx, `
		SELECT post FROM album_parts WHERE source = ? AND mid = ? AND post IN (?, ?)
	`, source.String(), event.Mid, event.Group, event.Group+"#"+event.Mid).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if _, err := s.ExecContext(ctx, `UPDATE album_parts SET payload = ? WHERE source = ? AND post = ? AND mid = ?`,
		[]byte(event.Message), source.String(), root, event.Mid); err != nil {
		return root, nil, err
	}
	payloads, err := s.albumPayloads(ctx, source, root)

	return root, payloads, err
}
