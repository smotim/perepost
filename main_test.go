package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

// A post that crashes every time must not restart the service forever: after
// maxPostFailures crashes it is skipped and the queue moves on.
func TestProcessSkipsAPostThatKeepsCrashing(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	src := channelKey{telegramPlatform, -42}
	for _, id := range []string{"1", "2"} {
		if err := db.enqueue(ctx, src, id, "message:"+id, []byte(id), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	intakeDone := make(chan struct{})
	close(intakeDone)
	var alerts, handled []string
	alert := func(_ context.Context, text string) error {
		alerts = append(alerts, text)

		return nil
	}
	forward := func(p pendingPost, _ *deliveryBook) {
		handled = append(handled, p.key)
		if p.key == "message:1" {
			panic("boom")
		}
	}

	// Each crash stops processing, as a restart follows it.
	for i := 1; i <= maxPostFailures; i++ {
		err := process(ctx, db, src, intakeDone, alert, forward)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("crash %d: expected an error carrying the panic, got %v", i, err)
		}
		if count, err := db.failures(ctx, src, "message:1"); err != nil || count != i {
			t.Fatalf("crash %d: failures = %d, %v", i, count, err)
		}
	}
	if err := process(ctx, db, src, intakeDone, alert, forward); err != nil {
		t.Fatal(err)
	}

	if want := []string{"message:1", "message:1", "message:1", "message:2"}; !slices.Equal(handled, want) {
		t.Errorf("handled %v, want %v", handled, want)
	}
	if len(alerts) != maxPostFailures+1 || !strings.Contains(alerts[len(alerts)-1], "пропущен") {
		t.Errorf("alerts = %q", alerts)
	}
	if count, err := db.failures(ctx, src, "message:1"); err != nil || count != 0 {
		t.Errorf("a skipped post must clear its failures: %d, %v", count, err)
	}
	if post, err := db.next(ctx, src, time.Now()); err != nil || post.row != 0 {
		t.Errorf("queue must be empty: %+v, %v", post, err)
	}
}

// A post that crashed once and then went through starts from zero next time.
func TestCompleteClearsFailures(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	src := channelKey{maxPlatform, -7}
	if err := db.enqueue(ctx, src, "m", "message:m", []byte("m"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.recordFailure(ctx, src, "message:m"); err != nil {
		t.Fatal(err)
	}
	post, err := db.next(ctx, src, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.complete(ctx, src, post); err != nil {
		t.Fatal(err)
	}
	if count, err := db.failures(ctx, src, "message:m"); err != nil || count != 0 {
		t.Errorf("failures = %d, %v", count, err)
	}
}

func TestProcessKeepsYoungAlbumQueuedOnShutdown(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	src := channelKey{telegramPlatform, -42}
	if err := db.enqueue(ctx, src, "1", "album:a", []byte(`{"message_id":1}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	intakeDone := make(chan struct{})
	close(intakeDone)
	forwarded := false
	err := process(ctx, db, src, intakeDone, func(context.Context, string) error { return nil }, func(pendingPost, *deliveryBook) {
		forwarded = true
	})
	if err != nil || forwarded {
		t.Fatalf("a young album must wait in the queue: forwarded=%v, err=%v", forwarded, err)
	}
	p, err := db.next(ctx, src, time.Now().Add(albumWait))
	if err != nil || p.key != "album:a" || len(p.payloads) != 1 {
		t.Fatalf("album lost on shutdown: %+v, %v", p, err)
	}
}

func TestProcessRelaysLateAlbumPartSeparatelyAfterRestart(t *testing.T) {
	for _, state := range []string{"done", "partial"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "queue.sqlite")
			db, err := openStore(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(db.close)
			firstAttempt, cancel := context.WithCancel(ctx)
			defer cancel()
			var texts []string
			r := &relay{max: &maxClient{baseURL: "https://max.example", publish: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					var message struct{ Text string }
					err := json.NewDecoder(req.Body).Decode(&message)
					_ = req.Body.Close()
					if err != nil {
						return nil, err
					}
					texts = append(texts, message.Text)
					body := io.NopCloser(strings.NewReader(fmt.Sprintf(`{"message":{"body":{"mid":"part.%d"}}}`, len(texts))))
					if state == "partial" && len(texts) == 1 {
						body = cancelOnClose{body, cancel}
					}
					return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header), Request: req}, nil
				}),
			}}}
			src := &source{channel: &models.ChatFullInfo{ID: -100}, to: []maxChat{{id: -42}}}
			source := channelKey{telegramPlatform, -100}
			destination := channelKey{maxPlatform, -42}
			received := time.Now().Add(-2 * albumWait)
			var original [][]byte
			for i, text := range []string{strings.Repeat("a", maxTextLimit), "original second part"} {
				id := fmt.Sprint(i + 1)
				payload := []byte(fmt.Sprintf(`{"message_id":%s,"text":%q}`, id, text))
				original = append(original, payload)
				if err := db.enqueue(ctx, source, id, "album:a", payload, received); err != nil {
					t.Fatal(err)
				}
			}
			book := &deliveryBook{store: db, source: source, post: "album:a"}
			p, err := db.next(ctx, source, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			r.forwardTelegramPost(firstAttempt, src, p, book)
			var saved string
			if err := db.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE source = ? AND post = 'album:a' AND destination = ?`, source.String(), destination.String()).Scan(&saved); book.err != nil || err != nil || saved != state {
				t.Fatalf("first attempt: state=%q, err=%v, book=%v", saved, err, book.err)
			}
			if state == "done" {
				if err := db.complete(ctx, source, p); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.enqueue(ctx, source, "3", "album:a", []byte(`{"message_id":3,"text":"late part"}`), received); err != nil {
				t.Fatal(err)
			}
			db.close()
			reopened, err := openStore(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reopened.close)
			intakeDone := make(chan struct{})
			close(intakeDone)
			forward := func(p pendingPost, book *deliveryBook) { r.forwardTelegramPost(ctx, src, p, book) }
			for range 2 {
				if err := process(ctx, reopened, source, intakeDone, r.alert, forward); err != nil {
					t.Fatal(err)
				}
			}
			if len(texts) != 3 || texts[0] != strings.Repeat("a", maxTextLimit) || texts[1] != "original second part" || texts[2] != "late part" {
				t.Fatalf("late part lost or original repeated: %q", texts)
			}
			if err := reopened.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE source = ? AND post = 'album:a#3' AND destination = ?`, source.String(), destination.String()).Scan(&saved); err != nil || saved != "done" {
				t.Fatalf("late part needs its own completed delivery: %q, %v", saved, err)
			}
			payloads, err := reopened.albumPayloads(ctx, source, "album:a")
			if err != nil || len(payloads) != 2 || string(payloads[0]) != string(original[0]) || string(payloads[1]) != string(original[1]) {
				t.Fatalf("original snapshot changed: %q, %v", payloads, err)
			}
		})
	}
}

func TestForwardTelegramPostKeepsKnownFieldsOnTypeError(t *testing.T) {
	f := fakes(t)
	db := testStore(t)
	p := pendingPost{key: "message:1", payloads: [][]byte{[]byte(`{"message_id":1,"message_thread_id":"unexpected","text":"keep this text"}`)}}
	book := &deliveryBook{store: db, source: channelKey{telegramPlatform, -100}, post: p.key}
	f.relay.forwardTelegramPost(t.Context(), f.source, p, book)
	messages := f.messages()
	if book.err != nil || len(messages) != 1 || messages[0].body.Text != "keep this text" {
		t.Fatalf("known fields were lost: %+v, %v", messages, book.err)
	}
	if alerts := f.alerted(); len(alerts) != 1 || !strings.Contains(alerts[0], "разобран не полностью") {
		t.Fatalf("partial parse must notify the admin: %q", alerts)
	}
}

func TestForwardMaxPostKeepsKnownFieldsOnTypeError(t *testing.T) {
	f := fakes(t)
	db := testStore(t)
	var text string
	telegram := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct{ Text string }
		err := json.NewDecoder(req.Body).Decode(&body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		text = body.Text
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":17}}`)), Header: make(http.Header), Request: req}, nil
	})}
	f.relay.tgAPI = telegramAPI{baseURL: "https://telegram.example", token: "token", http: telegram}
	src := &maxSource{channel: maxChat{id: -42}, to: []*models.ChatFullInfo{{ID: -100}}}
	p := pendingPost{key: "message:mid.1", payloads: [][]byte{[]byte(`{"recipient":{"chat_id":-42,"post_id":123},"body":{"mid":"mid.1","text":"keep MAX text"}}`)}}
	book := &deliveryBook{store: db, source: channelKey{maxPlatform, -42}, post: p.key}
	f.relay.forwardMaxPost(t.Context(), src, p, book)
	if book.err != nil || text != "keep MAX text" {
		t.Fatalf("known MAX fields were lost: %q, %v", text, book.err)
	}
	if alerts := f.alerted(); len(alerts) != 1 || !strings.Contains(alerts[0], "разобран не полностью") {
		t.Fatalf("partial MAX parse must notify the admin: %q", alerts)
	}
}

func TestTelegramMessagesDoesNotKeepMalformedJSON(t *testing.T) {
	messages, err := telegramMessages([][]byte{[]byte(`{"message_id":1,"text":"truncated"`), []byte(`{"message_id":2,"text":"valid"}`)})
	if err == nil || len(messages) != 1 || messages[0].ID != 2 || messages[0].Text != "valid" {
		t.Fatalf("malformed JSON must be skipped without losing valid parts: %+v, %v", messages, err)
	}
}

func TestTelegramEditRejectsPartialMessage(t *testing.T) {
	f := fakes(t)
	db := testStore(t)
	source := channelKey{telegramPlatform, -100}
	f.source.changes = map[int64]syncOptions{-42: {edits: true}}
	seedMirrors(t, db, source, channelKey{maxPlatform, -42}, "message:1", []string{"mid.original"}, mirrorText)
	event := change{Kind: telegramEdit, Mid: "1", Group: "message:1", At: 1, Sequence: 1,
		Message: json.RawMessage(`{"message_id":1,"message_thread_id":"unexpected","text":"incomplete edit"}`)}
	if err := db.enqueueChange(t.Context(), source, event); err != nil {
		t.Fatal(err)
	}
	p, book := nextChange(t, db, source)
	f.relay.forwardTelegramPost(t.Context(), f.source, p, book)
	if messages := f.messages(); len(messages) != 0 {
		t.Fatalf("a partial edit must not overwrite the published post: %+v", messages)
	}
	if alerts := f.alerted(); len(alerts) != 1 {
		t.Fatalf("rejected edit must notify the admin: %q", alerts)
	}
}

func TestTelegramLateAlbumEditTouchesOnlyItsOwnCopy(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	src := &source{channel: &models.ChatFullInfo{ID: -100}, to: []maxChat{{id: -42}}, changes: map[int64]syncOptions{-42: {edits: true}}}
	source := channelKey{telegramPlatform, -100}
	destination := channelKey{maxPlatform, -42}
	for _, part := range []struct{ key, id, text, mirror string }{
		{"album:a", "1", "original album", "mid.original"},
		{"album:a#2", "2", "late part", "mid.late"},
	} {
		payload := []byte(fmt.Sprintf(`{"message_id":%s,"caption":%q}`, part.id, part.text))
		if err := db.keepAlbum(ctx, source, part.key, [][]byte{payload}); err != nil {
			t.Fatal(err)
		}
		seedMirrors(t, db, source, destination, part.key, []string{part.mirror}, mirrorCaption)
	}
	var calls []string
	r := &relay{max: &maxClient{baseURL: "https://max.example", publish: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body struct{ Text string }
			err := json.NewDecoder(req.Body).Decode(&body)
			_ = req.Body.Close()
			if err != nil {
				return nil, err
			}
			calls = append(calls, req.Method+":"+req.URL.Query().Get("message_id")+":"+body.Text)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"success":true}`)), Header: make(http.Header), Request: req}, nil
		}),
	}}}
	event := change{Kind: telegramEdit, Mid: "2", Group: "album:a", At: 1, Sequence: 1,
		Message: json.RawMessage(`{"message_id":2,"caption":"edited late part"}`)}
	if err := db.enqueueChange(ctx, source, event); err != nil {
		t.Fatal(err)
	}
	p, book := nextChange(t, db, source)
	r.forwardTelegramPost(ctx, src, p, book)
	if book.err != nil || !slices.Equal(calls, []string{"PUT:mid.late:edited late part"}) {
		t.Fatalf("late edit must affect only the late copy: %q, %v", calls, book.err)
	}
}

func TestTelegramAlbumEditRejectsPartialMessageBeforeSaving(t *testing.T) {
	f := fakes(t)
	db := testStore(t)
	source := channelKey{telegramPlatform, -100}
	f.source.changes = map[int64]syncOptions{-42: {edits: true}}
	original := []byte(`{"message_id":1,"caption":"original caption"}`)
	if err := db.keepAlbum(t.Context(), source, "album:a", [][]byte{original}); err != nil {
		t.Fatal(err)
	}
	seedMirrors(t, db, source, channelKey{maxPlatform, -42}, "album:a", []string{"mid.original"}, mirrorCaption)
	event := change{Kind: telegramEdit, Mid: "1", Group: "album:a", At: 1, Sequence: 1,
		Message: json.RawMessage(`{"message_id":1,"message_thread_id":"unexpected","caption":"incomplete edit"}`)}
	if err := db.enqueueChange(t.Context(), source, event); err != nil {
		t.Fatal(err)
	}
	p, book := nextChange(t, db, source)
	f.relay.forwardTelegramPost(t.Context(), f.source, p, book)
	if messages := f.messages(); len(messages) != 0 {
		t.Fatalf("a partial edit must not overwrite the published album: %+v", messages)
	}
	payloads, err := db.albumPayloads(t.Context(), source, "album:a")
	if err != nil || len(payloads) != 1 || string(payloads[0]) != string(original) {
		t.Fatalf("a partial edit must not replace the saved album: %q, %v", payloads, err)
	}
	if alerts := f.alerted(); len(alerts) != 1 {
		t.Fatalf("rejected edit must notify the admin: %q", alerts)
	}
}

func TestForwardMalformedPostWarnsThatItWasSkipped(t *testing.T) {
	for _, platform := range []platform{telegramPlatform, maxPlatform} {
		t.Run(string(platform), func(t *testing.T) {
			f := fakes(t)
			p := pendingPost{key: "message:1", payloads: [][]byte{[]byte(`{"body":`)}}
			book := &deliveryBook{store: testStore(t), source: channelKey{platform, -100}, post: p.key}
			if platform == telegramPlatform {
				f.relay.forwardTelegramPost(t.Context(), f.source, p, book)
			} else {
				f.relay.forwardMaxPost(t.Context(), &maxSource{}, p, book)
			}
			if alerts := f.alerted(); len(alerts) != 1 || !strings.Contains(alerts[0], "пост пропущен") || strings.Contains(alerts[0], "переносим") {
				t.Fatalf("malformed post needs an accurate warning: %q", alerts)
			}
		})
	}
}

func TestProcessCancellationWhileCompletingSkippedPostIsClean(t *testing.T) {
	db := testStore(t)
	source := channelKey{telegramPlatform, -100}
	if err := db.enqueue(t.Context(), source, "1", "message:1", []byte(`{"message_id":1}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	for range maxPostFailures {
		if _, err := db.recordFailure(t.Context(), source, "message:1"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	intakeDone := make(chan struct{})
	close(intakeDone)
	err := process(ctx, db, source, intakeDone, func(context.Context, string) error {
		cancel()
		return nil
	}, func(pendingPost, *deliveryBook) { t.Error("a repeatedly crashing post must be skipped") })
	if err != nil {
		t.Fatalf("shutdown during completion must be clean: %v", err)
	}
	p, err := db.next(t.Context(), source, time.Now())
	if err != nil || p.key != "message:1" {
		t.Fatalf("unfinished cleanup must remain queued: %+v, %v", p, err)
	}
}

func TestProcessDoesNotHideDeliveryStorageErrorOnCancellation(t *testing.T) {
	db := testStore(t)
	source := channelKey{telegramPlatform, -100}
	if err := db.enqueue(t.Context(), source, "1", "message:1", []byte(`{"message_id":1}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	want := errors.New("failed to save publication state")
	err := process(ctx, db, source, make(chan struct{}), func(context.Context, string) error { return nil }, func(_ pendingPost, book *deliveryBook) {
		book.err = want
		cancel()
	})
	if !errors.Is(err, want) {
		t.Fatalf("shutdown must not hide an unrelated storage failure: %v", err)
	}
}
