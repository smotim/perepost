package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

func TestMaxWebhookPersistsAndRelaysAlbumOnce(t *testing.T) {
	ctx := t.Context()
	db := testStore(t)
	src := &maxSource{channel: maxChat{id: -42, title: "MAX"}, to: []*models.ChatFullInfo{{ID: -100, Title: "Telegram"}}}
	key := channelKey{maxPlatform, src.channel.id}
	hook := maxWebhook("test-secret", db, map[int64]*maxSource{-42: src})
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	photo := pngBytes.Bytes()
	video := []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/image" {
			_, _ = w.Write(photo)
		} else {
			_, _ = w.Write(video)
		}
	}))
	defer cdn.Close()
	payload := fmt.Sprintf(`{
		"update_type":"message_created",
		"message":{"recipient":{"chat_type":"channel","chat_id":-42},
		"body":{"mid":"mid.1","text":"🙂 test","markup":[{"type":"strong","from":3,"length":4}],
		"attachments":[{"type":"image","payload":{"url":%q}},{"type":"video","payload":{"url":%q}}]}}
	}`, cdn.URL+"/image", cdn.URL+"/video")
	for i, secret := range []string{"wrong", "test-secret", "test-secret"} {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/perepost/max", strings.NewReader(payload))
		req.Header.Set("X-Max-Bot-Api-Secret", secret)
		w := httptest.NewRecorder()
		hook.ServeHTTP(w, req)
		want := http.StatusOK
		if i == 0 {
			want = http.StatusUnauthorized
		}
		if w.Code != want {
			t.Fatalf("webhook status %d, want %d", w.Code, want)
		}
	}
	post, err := db.next(ctx, key, time.Now())
	if err != nil || len(post.payloads) != 1 {
		t.Fatalf("webhook not stored exactly once: %+v, %v", post, err)
	}
	var calls atomic.Int32
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.URL.Path != "/bottoken/sendMediaGroup" {
			t.Errorf("method %s", req.URL.Path)
		}
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("invalid multipart: %v", err)

			return
		}
		defer func() { _ = req.MultipartForm.RemoveAll() }()
		for i, want := range [][]byte{photo, video} {
			file, _, err := req.FormFile(fileField(i))
			if err != nil {
				t.Error(err)

				continue
			}
			contents, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil || !bytes.Equal(contents, want) {
				t.Errorf("file %d: %q, %v", i, contents, err)
			}
		}
		var album []struct {
			Type     string                 `json:"type"`
			Caption  string                 `json:"caption"`
			Entities []models.MessageEntity `json:"caption_entities"`
		}
		if err := json.Unmarshal([]byte(req.FormValue("media")), &album); err != nil || len(album) != 2 {
			t.Errorf("album: %+v, %v", album, err)
		} else if album[0].Type != "photo" || album[1].Type != "video" || album[0].Caption != "🙂 test" ||
			len(album[0].Entities) != 1 || album[0].Entities[0].Offset != 3 || album[0].Entities[0].Type != models.MessageEntityTypeBold {
			t.Errorf("album contents: %+v", album)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[{"message_id":11},{"message_id":12}]}`)
	}))
	defer telegram.Close()
	dir := t.TempDir()
	r := &relay{tgAPI: telegramAPI{baseURL: telegram.URL, token: "token", http: telegram.Client()}, maxMediaHTTP: cdn.Client(),
		mediaDir: dir, maxFileLimit: 1024, maxPostLimit: 2048, mediaSlot: make(chan struct{}, 1)}
	var m maxMessage
	if err := json.Unmarshal(post.payloads[0], &m); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		book := &deliveryBook{store: db, source: key, post: post.key}
		r.forwardMax(ctx, src, m, book)
		if book.err != nil {
			t.Fatal(book.err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("published %d times", calls.Load())
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("temporary files remain: %v, %v", entries, err)
	}
	if err := db.complete(ctx, key, post); err != nil {
		t.Fatal(err)
	}
	if p, err := db.next(ctx, key, time.Now()); err != nil || p.row != 0 {
		t.Fatalf("duplicate webhook in queue: %+v, %v", p, err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM published WHERE channel = 'telegram:-100' AND mid IN ('11', '12')`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("missing album output IDs: %d, %v", count, err)
	}
}

func TestTelegramAcknowledgesOnlyAfterPersisting(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	db := testStore(t)
	key := channelKey{telegramPlatform, -100}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload struct {
			Offset int64 `json:"offset"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Error(err)
			cancel()

			return
		}
		switch payload.Offset {
		case 0:
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":42,"channel_post":{"message_id":8,"chat":{"id":-100,"type":"channel"},"text":"example"}}]}`)
		case 43:
			p, err := db.next(req.Context(), key, time.Now())
			if err != nil || p.key != "message:8" {
				t.Errorf("acknowledging before persisting: %+v, %v", p, err)
			}
			cancel()
		default:
			t.Errorf("unexpected offset %d", payload.Offset)
			cancel()
		}
	}))
	defer server.Close()
	api := telegramAPI{baseURL: server.URL, token: "token", http: server.Client()}
	if err := api.poll(ctx, db, map[int64]*source{-100: {}}, []string{"channel_post"}); err != nil {
		t.Fatal(err)
	}
}
