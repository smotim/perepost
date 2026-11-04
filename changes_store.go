package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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

func (s *store) reviseAlbum(ctx context.Context, source channelKey, event change) ([][]byte, error) {
	if _, err := s.ExecContext(ctx, `UPDATE album_parts SET payload = ? WHERE source = ? AND post = ? AND mid = ?`,
		[]byte(event.Message), source.String(), event.Group, event.Mid); err != nil {
		return nil, err
	}
	rows, err := s.QueryContext(ctx, `SELECT payload FROM album_parts WHERE source = ? AND post = ? ORDER BY CAST(mid AS INTEGER)`,
		source.String(), event.Group)
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
