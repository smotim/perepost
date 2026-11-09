package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func TestOldTelegramCopiesReplaceStaleContentsWhenDeletionIsForbidden(t *testing.T) {
	for _, kind := range []mirrorKind{mirrorText, mirrorCaption, mirrorMedia} {
		t.Run(string(kind), func(t *testing.T) {
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				method := strings.TrimPrefix(req.URL.Path, "/bottoken/")
				calls = append(calls, method)
				if method == "deleteMessage" {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message can't be deleted for everyone"}`)
					return
				}
				var payload map[string]json.RawMessage
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				field := "text"
				if kind != mirrorText {
					field = "caption"
				}
				if string(payload[field]) != `"Содержимое удалено."` {
					t.Errorf("payload=%s", payload[field])
				}
				fmt.Fprint(w, `{"ok":true,"result":true}`)
			}))
			t.Cleanup(server.Close)
			api := telegramAPI{baseURL: server.URL, token: "token", http: server.Client()}
			if err := api.deleteMirror(t.Context(), -100, mirrorPart{id: "123", kind: kind}); err != nil {
				t.Fatal(err)
			}
			edit := "editMessageText"
			if kind != mirrorText {
				edit = "editMessageCaption"
			}
			if !slices.Equal(calls, []string{"deleteMessage", edit}) {
				t.Fatalf("calls=%v", calls)
			}
		})
	}
}

func TestMissingTelegramCopyDoesNotCreateTombstone(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		if !strings.HasSuffix(req.URL.Path, "/deleteMessage") {
			t.Error("unexpected edit")
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message to delete not found"}`)
	}))
	t.Cleanup(server.Close)
	api := telegramAPI{baseURL: server.URL, token: "token", http: server.Client()}
	if err := api.deleteMirror(t.Context(), -100, mirrorPart{id: "123", kind: mirrorText}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
}

func submitHook(t *testing.T, hook http.Handler, header, secret, body string, want int) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set(header, secret)
	res := httptest.NewRecorder()
	hook.ServeHTTP(res, req)
	if res.Code != want {
		t.Fatalf("webhook status=%d, want %d", res.Code, want)
	}
}

func nextChange(t *testing.T, db *store, source channelKey) (pendingPost, *deliveryBook) {
	t.Helper()
	p, err := db.next(t.Context(), source, time.Now().Add(albumWait))
	if err != nil || !strings.HasPrefix(p.key, "change:") {
		t.Fatalf("expected a queued change, got %+v, %v", p, err)
	}

	return p, &deliveryBook{store: db, source: source, post: p.key}
}

func seedMirrors(t *testing.T, db *store, source, destination channelKey, root string, ids []string, kinds ...mirrorKind) {
	t.Helper()
	book := &deliveryBook{store: db, source: source, post: root}
	if err := book.set(t.Context(), destination, "done", ids, kinds...); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedRevisionAppendsOnlyItsMissingTail(t *testing.T) {
	db := testStore(t)
	src, dst := channelKey{telegramPlatform, -100}, channelKey{maxPlatform, -42}
	seedMirrors(t, db, src, dst, "message:8", []string{"original"}, mirrorText)
	book := &deliveryBook{store: db, source: src, post: "change:8", root: "message:8"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var appended []string
	syncText := func(ctx context.Context, stop bool) error {
		return syncMirrorText(ctx, book, dst, func(bool) (string, []string) {
			return "", []string{"first", "second", "third"}
		}, func(mirrorPart, string) error { return nil }, func(mirrorPart) error {
			t.Error("no part should be removed")
			return nil
		}, func(text string, publish publish) error {
			return publish(ctx, func() ([]string, error) {
				appended = append(appended, text)
				if stop {
					cancel()
				}
				return []string{text}, nil
			}, mirrorText)
		})
	}
	if err := syncText(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("revision was not interrupted between parts: %v", err)
	}
	if err := syncText(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(appended, []string{"second", "third"}) {
		t.Fatalf("revision appended %v", appended)
	}
}

func TestTelegramWebhookEditsSplitCopyInPlaceOnlyForEnabledPair(t *testing.T) {
	ctx := t.Context()
	db := testStore(t)
	sourceKey := channelKey{telegramPlatform, -100}
	maxOne, maxTwo := channelKey{maxPlatform, -42}, channelKey{maxPlatform, -43}
	src := &source{
		channel: &models.ChatFullInfo{ID: -100, Title: "source"},
		to:      []maxChat{{id: -42}, {id: -43}},
		changes: map[int64]syncOptions{-42: {edits: true}},
	}
	hook := telegramWebhook("secret", db, map[int64]*source{-100: src})
	header := "X-Telegram-Bot-Api-Secret-Token"
	submitHook(t, hook, header, "wrong", "{}", http.StatusUnauthorized)
	postJSON := `{"update_id":21,"channel_post":{"message_id":8,"chat":{"id":-100,"type":"channel"},"text":"original"}}`
	for range 2 {
		submitHook(t, hook, header, "secret", postJSON, http.StatusOK)
	}
	p, err := db.next(ctx, sourceKey, time.Now())
	if err != nil || p.key != "message:8" || len(p.payloads) != 1 {
		t.Fatalf("original not saved exactly once: %+v %v", p, err)
	}
	if err := db.complete(ctx, sourceKey, p); err != nil {
		t.Fatal(err)
	}
	seedMirrors(t, db, sourceKey, maxOne, p.key, []string{"mid.first"}, mirrorText)
	seedMirrors(t, db, sourceKey, maxTwo, p.key, []string{"mid.disabled"}, mirrorText)

	var calls []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		id := req.URL.Query().Get("message_id")
		switch req.Method {
		case http.MethodPut:
			calls = append(calls, "edit:"+id)
			var payload map[string]json.RawMessage
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil || payload["text"] == nil || payload["attachments"] != nil {
				t.Errorf("editing the body should keep existing media: %+v, %v", payload, err)
			}
			fmt.Fprint(w, `{"success":true}`)
		case http.MethodPost:
			var payload struct {
				Notify *bool `json:"notify"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.Notify != nil && !*payload.Notify {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"code":"proto.payload","message":"errors.send-message.channel-notify"}`)

				return
			}
			calls = append(calls, "add:"+req.URL.Query().Get("chat_id"))
			fmt.Fprint(w, `{"message":{"body":{"mid":"mid.extra"}}}`)
		case http.MethodDelete:
			calls = append(calls, "delete:"+id)
			fmt.Fprint(w, `{"success":true}`)
		}
	}))
	defer server.Close()
	r := &relay{max: &maxClient{baseURL: server.URL, publish: server.Client()}}

	for _, revision := range []struct {
		seq, stamp int
		text       string
	}{
		{22, 100, strings.Repeat("word & ", 500)},
		{23, 101, "short again"},
		{24, 99, "stale"},
	} {
		payload := fmt.Sprintf(`{"update_id":%d,"edited_channel_post":{"message_id":8,"edit_date":%d,"chat":{"id":-100,"type":"channel"},"text":%q}}`,
			revision.seq, revision.stamp, revision.text)
		for range 2 {
			submitHook(t, hook, header, "secret", payload, http.StatusOK)
		}
		p, book := nextChange(t, db, sourceKey)
		r.editFromTelegram(ctx, src, p, book)
		if book.err != nil {
			t.Fatal(book.err)
		}
		if err := db.complete(ctx, sourceKey, p); err != nil {
			t.Fatal(err)
		}
		if p, err := db.next(ctx, sourceKey, time.Now()); err != nil || p.row != 0 {
			t.Fatalf("unexpected duplicate change: %+v, %v", p, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"edit:mid.first", "add:-42", "edit:mid.first", "delete:mid.extra"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls=%v, want %v", calls, want)
	}
}

func TestMaxWebhookEditsCaptionAndDeletesOnlyEnabledMirrors(t *testing.T) {
	ctx := t.Context()
	db := testStore(t)
	sourceKey := channelKey{maxPlatform, -42}
	one, two := channelKey{telegramPlatform, -100}, channelKey{telegramPlatform, -101}
	src := &maxSource{
		channel: maxChat{id: -42},
		to:      []*models.ChatFullInfo{{ID: -100}, {ID: -101}},
		changes: map[int64]syncOptions{-100: {edits: true, deletes: true}, -101: {edits: true}},
	}
	seedMirrors(t, db, sourceKey, one, "message:mid.source", []string{"11", "12", "13"}, mirrorCaption, mirrorMedia, mirrorText)
	seedMirrors(t, db, sourceKey, two, "message:mid.source", []string{"21"}, mirrorCaption)
	hook := maxWebhook("secret", db, map[int64]*maxSource{-42: src})
	var calls []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			ID      int    `json:"message_id"`
			Caption string `json:"caption"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		method := strings.TrimPrefix(req.URL.Path, "/bottoken/")
		mu.Lock()
		calls = append(calls, fmt.Sprintf("%s:%d", method, body.ID))
		mu.Unlock()
		if method == "editMessageCaption" && body.Caption != "edited" {
			t.Errorf("caption=%q", body.Caption)
		}
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	}))
	defer server.Close()
	r := &relay{tgAPI: telegramAPI{baseURL: server.URL, token: "token", http: server.Client()}}
	for _, payload := range []string{
		`{"update_type":"message_edited","timestamp":10,"message":{"recipient":{"chat_type":"channel","chat_id":-42},"body":{"mid":"mid.source","text":"edited"}}}`,
		`{"update_type":"message_removed","timestamp":11,"chat_id":-42,"message_id":"mid.source"}`,
	} {
		submitHook(t, hook, "X-Max-Bot-Api-Secret", "secret", payload, http.StatusOK)
		p, book := nextChange(t, db, sourceKey)
		r.changeFromMax(ctx, src, p, book)
		if book.err != nil {
			t.Fatal(book.err)
		}
		if err := db.complete(ctx, sourceKey, p); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"editMessageCaption:11", "deleteMessage:13", "editMessageCaption:21", "deleteMessage:11", "deleteMessage:12"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls=%v, want %v", calls, want)
	}
}

func TestTelegramWebhookRegistrationKeepsPendingUpdatesAndRefusesAnotherURL(t *testing.T) {
	ctx := t.Context()
	address := "https://example.test/perepost/telegram"
	var oldURL string
	var configured map[string]string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch req.URL.Path {
		case "/bottoken/getWebhookInfo":
			fmt.Fprintf(w, `{"ok":true,"result":{"url":%q}}`, oldURL)
		case "/bottoken/setWebhook":
			if err := req.ParseMultipartForm(1024); err != nil {
				t.Error(err)
			}
			configured = map[string]string{}
			for _, key := range []string{"url", "allowed_updates", "drop_pending_updates", "secret_token", "max_connections"} {
				configured[key] = req.FormValue(key)
			}
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	defer server.Close()
	tg, err := bot.New("token", bot.WithSkipGetMe(), bot.WithServerURL(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	events := []string{"channel_post", telegramEdit}
	if err := configureTelegramWebhook(ctx, tg, address, "secret", events, true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	want := map[string]string{"url": address, "allowed_updates": `["channel_post","edited_channel_post"]`, "drop_pending_updates": "", "secret_token": "secret", "max_connections": "1"}
	if !reflect.DeepEqual(configured, want) {
		t.Errorf("registration params=%v, want %v", configured, want)
	}
	oldURL, configured = "https://example.test/foreign", nil
	mu.Unlock()
	if err := configureTelegramWebhook(ctx, tg, address, "secret", events, true); err == nil {
		t.Fatal("another webhook should be preserved")
	}
	mu.Lock()
	defer mu.Unlock()
	if configured != nil {
		t.Fatalf("foreign webhook was modified: %v", configured)
	}
}
