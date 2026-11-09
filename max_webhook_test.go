package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMaxWebhookReadsOnlyRoutingFields(t *testing.T) {
	for _, tc := range []struct {
		name, recipient, body, mid string
	}{
		{"body ID takes precedence", `"post_id":123`, `"mid":"mid.1","text":"post"`, "mid.1"},
		{"numeric fallback keeps precision", `"post_id":9007199254740993`, `"text":"post"`, "9007199254740993"},
		{"string fallback", `"post_id":"mid.fallback"`, `"text":"post"`, "mid.fallback"},
		{"irrelevant recipient field", `"user_id":"not-an-integer"`, `"mid":"mid.2","text":"post"`, "mid.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testStore(t)
			src := &maxSource{channel: maxChat{id: -42}}
			hook := maxWebhook("secret", db, map[int64]*maxSource{-42: src})
			payload := fmt.Sprintf(`{"update_type":"message_created","message":{"recipient":{"chat_id":-42,"chat_type":"channel",%s},"body":{%s}}}`, tc.recipient, tc.body)
			for range 2 {
				submitHook(t, hook, "X-Max-Bot-Api-Secret", "secret", payload, http.StatusOK)
			}
			post, err := db.next(t.Context(), channelKey{maxPlatform, -42}, time.Now())
			if err != nil || post.key != "message:"+tc.mid || len(post.payloads) != 1 {
				t.Fatalf("post not persisted exactly once: key=%q, payloads=%d, err=%v", post.key, len(post.payloads), err)
			}
		})
	}
}

func TestMaxWebhookLogsMalformedEventWithoutPayload(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	db := testStore(t)
	hook := maxWebhook("secret", db, map[int64]*maxSource{-42: {channel: maxChat{id: -42}}})
	for _, tc := range []struct {
		name, payload string
	}{
		{"envelope", `{"update_type":"message_created","timestamp":"invalid","message":{"body":{"text":"private-post-contents"}}}`},
		{"routing fields", `{"update_type":"message_created","message":{"recipient":{"chat_id":"invalid","chat_type":"channel"},"body":{"mid":"mid.1","text":"private-post-contents"}}}`},
		{"fallback ID", `{"update_type":"message_created","message":{"recipient":{"chat_id":-42,"chat_type":"channel","post_id":true},"body":{"text":"private-post-contents"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output.Reset()
			submitHook(t, hook, "X-Max-Bot-Api-Secret", "secret", tc.payload, http.StatusBadRequest)
			log := output.String()
			if !strings.Contains(log, "message_created") || !strings.Contains(log, "cannot unmarshal") {
				t.Fatalf("missing event type or decode error: %q", log)
			}
			if strings.Contains(log, "private-post-contents") || strings.Contains(log, tc.payload) {
				t.Fatalf("payload leaked: %q", log)
			}
		})
	}
	if post, err := db.next(t.Context(), channelKey{maxPlatform, -42}, time.Now()); err != nil || post.row != 0 {
		t.Fatalf("invalid posts must not be queued: row=%d, err=%v", post.row, err)
	}
}
