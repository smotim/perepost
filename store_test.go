package main

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func testStore(t *testing.T) *store {
	t.Helper()
	db, err := openStore(t.Context(), filepath.Join(t.TempDir(), "queue.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.close)

	return db
}

func TestPersistedAlbumKeepsItsPlace(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	src := channelKey{telegramPlatform, -42}
	now := time.Now()
	for _, item := range []struct{ id, post string }{
		{"1", "message:1"}, {"2", "album:a"}, {"3", "album:a"}, {"4", "message:4"}, {"5", "album:a"}, {"3", "album:a"},
	} {
		if err := db.enqueue(ctx, src, item.id, item.post, []byte(item.id), now); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range [][]string{{"1"}, {"2", "3", "5"}, {"4"}} {
		if i == 1 {
			post, err := db.next(ctx, src, now)
			if err != nil || post.row != 0 {
				t.Fatalf("album should hold the next post back: %+v, %v", post, err)
			}
		}
		post, err := db.next(ctx, src, now.Add(albumWait+time.Second))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, p := range post.payloads {
			got = append(got, string(p))
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		if err := db.complete(ctx, src, post); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWebhookEchoArrivingBeforePublishResponseIsSuppressed(t *testing.T) {
	ctx := t.Context()
	db := testStore(t)
	src, dst := channelKey{telegramPlatform, -100}, channelKey{maxPlatform, -42}
	book := &deliveryBook{store: db, source: src, post: "message:8"}
	err := book.deliver(ctx, dst, func(publish publish) error {
		return publish(ctx, func() ([]string, error) {
			for _, id := range []string{"mid.first", "mid.second"} {
				if err := db.enqueue(ctx, dst, id, "message:"+id, []byte(id), time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if post, err := db.next(ctx, dst, time.Now()); err != nil || post.row != 0 {
				t.Fatalf("a publication still in flight must hold back its echo: %+v, %v", post, err)
			}

			return []string{"mid.first", "mid.second"}, nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		post, err := db.next(ctx, dst, time.Now())
		if err != nil || post.row == 0 || len(post.payloads) != 0 {
			t.Fatalf("expected an echo with no relay payload: %+v, %v", post, err)
		}
		if err := db.complete(ctx, dst, post); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRestartPreservesQueueAndDoesNotRepublishAnUncertainDestination(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "queue.sqlite")
	db, err := openStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.close)
	src, dst := channelKey{telegramPlatform, -100}, channelKey{maxPlatform, -42}
	key := "message:8"
	if err := db.enqueue(ctx, src, "8", key, []byte("original"), time.Now()); err != nil {
		t.Fatal(err)
	}
	book := &deliveryBook{store: db, source: src, post: key}
	if err := book.set(ctx, dst, "sending", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.enqueue(ctx, dst, "mid.echo", "message:mid.echo", []byte("echo"), time.Now()); err != nil {
		t.Fatal(err)
	}
	db.close() // emulate a process crash after the POST and before its response

	reopened, err := openStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	p, err := reopened.next(ctx, src, time.Now())
	if err != nil || len(p.payloads) != 1 || string(p.payloads[0]) != "original" {
		t.Fatalf("source post not restored: %+v, %v", p, err)
	}
	book.store = reopened
	other := channelKey{maxPlatform, -43}
	if err := book.deliver(ctx, dst, func(publish) error {
		t.Fatal("destination with unknown outcome must not be replayed")

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := book.deliver(ctx, other, func(publish publish) error {
		return publish(ctx, func() ([]string, error) { return []string{"mid.other"}, nil })
	}); err != nil {
		t.Fatal(err)
	}
	if p, err := reopened.next(ctx, dst, time.Now()); err != nil || p.row != 0 {
		t.Fatalf("an unresolved destination can still echo: %+v, %v", p, err)
	}
	if err := reopened.resolveUncertain(ctx, src.String(), key, dst.String(), []string{"mid.echo"}); err != nil {
		t.Fatal(err)
	}
	if p, err := reopened.next(ctx, dst, time.Now()); err != nil || p.row == 0 || len(p.payloads) != 0 {
		t.Fatalf("echo should be discarded after resolution: %+v, %v", p, err)
	}
}

func TestPublishDoesNotRetryAnUnknownOutcome(t *testing.T) {
	unknown := errors.New("response lost")
	attempts := 0
	_, err := retryPublish(context.Background(), func() ([]string, error) {
		attempts++

		return nil, unknown
	})
	if !errors.Is(err, unknown) || attempts != 1 {
		t.Fatalf("attempts=%d, err=%v", attempts, err)
	}
}
