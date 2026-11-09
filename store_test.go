package main

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
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
	if err := reopened.resolveDelivery(ctx, src.String(), key, dst.String(), []string{"mid.echo"}); err != nil {
		t.Fatal(err)
	}
	if p, err := reopened.next(ctx, dst, time.Now()); err != nil || p.row == 0 || len(p.payloads) != 0 {
		t.Fatalf("echo should be discarded after resolution: %+v, %v", p, err)
	}
}

func TestPublishDoesNotRetryAnUnknownOutcome(t *testing.T) {
	unknown := errors.New("response lost")
	attempts := 0
	_, err := retryPublish(t.Context(), func() ([]string, error) {
		attempts++

		return nil, unknown
	})
	if !errors.Is(err, unknown) || attempts != 1 {
		t.Fatalf("attempts=%d, err=%v", attempts, err)
	}
}

func TestInterruptedMultipartDeliveryResumesAfterRestart(t *testing.T) {
	for _, first := range [][]string{{"one"}, {"photo", "video"}} {
		t.Run(strings.Join(first, "-"), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			path := filepath.Join(t.TempDir(), "queue.sqlite")
			db, err := openStore(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.close() })
			src, dst := channelKey{maxPlatform, -42}, channelKey{telegramPlatform, -100}
			book := &deliveryBook{store: db, source: src, post: "message:original"}
			var sent []string
			parts := [][]string{first, {"tail"}}
			send := func(ctx context.Context, stop bool) func(publish) error {
				return func(publish publish) error {
					for i, ids := range parts {
						kinds := make([]mirrorKind, len(ids))
						for j := range kinds {
							kinds[j] = mirrorText
						}
						if err := publish(ctx, func() ([]string, error) {
							sent = append(sent, ids...)
							if stop && i == 0 {
								cancel()
							}
							return ids, nil
						}, kinds...); err != nil {
							return err
						}
					}
					return nil
				}
			}
			if err := book.deliver(ctx, dst, send(ctx, true)); !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted delivery: %v", err)
			}
			db.close()
			db, err = openStore(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			book = &deliveryBook{store: db, source: src, post: "message:original"}
			if err := book.deliver(t.Context(), dst, send(t.Context(), false)); err != nil {
				t.Fatal(err)
			}
			if want := append(slices.Clone(first), "tail"); !slices.Equal(sent, want) {
				t.Fatalf("published %v, want each part once: %v", sent, want)
			}
			var state string
			if err := db.QueryRowContext(t.Context(), `SELECT state FROM deliveries WHERE source = ? AND post = ? AND destination = ?`, src.String(), book.post, dst.String()).Scan(&state); err != nil || state != "done" {
				t.Fatalf("final state %q: %v", state, err)
			}
		})
	}
}

func TestCanceledPublicationKeepsUnknownOutcomeBlocked(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	src, dst := channelKey{telegramPlatform, -100}, channelKey{maxPlatform, -42}
	book := &deliveryBook{store: db, source: src, post: "message:8"}
	err := book.deliver(ctx, dst, func(publish publish) error {
		return publish(ctx, func() ([]string, error) {
			cancel() // the request may already have reached the platform
			return nil, context.Canceled
		}, mirrorText)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
	var state string
	if err := db.QueryRowContext(t.Context(), `SELECT state FROM deliveries`).Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("unknown publication must remain uncertain: %q, %v", state, err)
	}
	if err := book.deliver(t.Context(), dst, func(publish) error {
		t.Fatal("unknown outcome must never be retried")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledRejectedPublicationCanResume(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	src, dst := channelKey{telegramPlatform, -100}, channelKey{maxPlatform, -42}
	book := &deliveryBook{store: db, source: src, post: "message:8"}
	err := book.deliver(ctx, dst, func(publish publish) error {
		return publish(ctx, func() ([]string, error) {
			cancel() // cancellation while pacing or waiting after an explicit 429
			return nil, &publishError{message: "cancelled before publication"}
		}, mirrorText)
	})
	if !errors.Is(err, errRejected) {
		t.Fatalf("expected known rejection: %v", err)
	}
	sent := false
	if err := book.deliver(t.Context(), dst, func(publish publish) error {
		return publish(t.Context(), func() ([]string, error) {
			sent = true
			return []string{"one"}, nil
		}, mirrorText)
	}); err != nil || !sent {
		t.Fatalf("known non-publication must resume: sent=%t, err=%v", sent, err)
	}
}

func TestResumeRejectsChangedPublicationPlan(t *testing.T) {
	for _, change := range []struct {
		name string
		plan []plannedMessage
	}{
		{"text", []plannedMessage{{Text: "changed"}}},
		{"media", []plannedMessage{{Text: "original", Media: []plannedMedia{{"other", "image"}}}}},
		{"boundaries", []plannedMessage{{Text: "orig"}, {Text: "inal"}}},
	} {
		t.Run(change.name, func(t *testing.T) {
			db := testStore(t)
			dst := channelKey{maxPlatform, -42}
			book := &deliveryBook{store: db, source: channelKey{telegramPlatform, -100}, post: "message:8"}
			if err := book.checkPlan(t.Context(), dst, []plannedMessage{{Text: "original"}}); err != nil {
				t.Fatal(err)
			}
			if err := book.set(t.Context(), dst, "partial", []string{"one"}, mirrorText); err != nil {
				t.Fatal(err)
			}
			if err := book.checkPlan(t.Context(), dst, change.plan); err == nil {
				t.Fatal("a different plan must not reuse the saved prefix")
			}
			if err := book.deliver(t.Context(), dst, func(publish) error {
				t.Fatal("changed plan must not publish")
				return nil
			}); err == nil {
				t.Fatal("plan failure must stop the worker before acknowledging the post")
			}
			var state string
			if err := db.QueryRowContext(t.Context(), `SELECT state FROM deliveries`).Scan(&state); err != nil || state != "partial" {
				t.Fatalf("saved prefix lost: %q, %v", state, err)
			}
		})
	}
}

func TestLegacyPartialWithoutPlanIsNotResumedBlindly(t *testing.T) {
	db := testStore(t)
	dst := channelKey{maxPlatform, -42}
	book := &deliveryBook{store: db, source: channelKey{telegramPlatform, -100}, post: "message:8"}
	if err := book.set(t.Context(), dst, "partial", []string{"one"}, mirrorText); err != nil {
		t.Fatal(err)
	}
	if err := book.checkPlan(t.Context(), dst, []plannedMessage{{Text: "original"}}); err == nil {
		t.Fatal("legacy partial has no evidence that the plan is still the same")
	}
}

func TestUnpublishedDeliveryCanUpdateItsPlan(t *testing.T) {
	for _, state := range []string{"", "partial"} {
		t.Run("state="+state, func(t *testing.T) {
			db := testStore(t)
			dst := channelKey{maxPlatform, -42}
			book := &deliveryBook{store: db, source: channelKey{telegramPlatform, -100}, post: "message:8"}
			if err := book.checkPlan(t.Context(), dst, []plannedMessage{{Text: "fallback"}}); err != nil {
				t.Fatal(err)
			}
			if state != "" {
				if err := book.set(t.Context(), dst, state, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := book.checkPlan(t.Context(), dst, []plannedMessage{{Text: "original", Media: []plannedMedia{{"photo", "image"}}}}); err != nil {
				t.Fatalf("a plan with no published parts can safely change: %v", err)
			}
		})
	}
}

func TestResumeDoesNotSplitAnAlreadyPublishedGroup(t *testing.T) {
	db := testStore(t)
	dst := channelKey{telegramPlatform, -100}
	book := &deliveryBook{store: db, source: channelKey{maxPlatform, -42}, post: "message:8"}
	if err := book.set(t.Context(), dst, "partial", []string{"photo"}, mirrorCaption); err != nil {
		t.Fatal(err)
	}
	err := book.deliver(t.Context(), dst, func(publish publish) error {
		return publish(t.Context(), func() ([]string, error) {
			t.Fatal("a changed group boundary must not duplicate the published photo")
			return nil, nil
		}, mirrorCaption, mirrorMedia)
	})
	if err == nil || book.err == nil {
		t.Fatalf("changed group must stop processing: %v, %v", err, book.err)
	}
}

func TestManualResolutionPreservesKnownCopiesAndRecordsManualIDs(t *testing.T) {
	for _, state := range []string{"partial", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			db := testStore(t)
			src, dst := channelKey{telegramPlatform, -100}, channelKey{maxPlatform, -42}
			book := &deliveryBook{store: db, source: src, post: "message:8"}
			if err := book.set(t.Context(), dst, state, []string{"known"}, mirrorCaption); err != nil {
				t.Fatal(err)
			}
			if err := db.resolveDelivery(t.Context(), src.String(), book.post, dst.String(), []string{"known", "manual"}); err != nil {
				t.Fatal(err)
			}
			var id string
			var kind mirrorKind
			if err := db.QueryRowContext(t.Context(), `SELECT mid, kind FROM mirrors`).Scan(&id, &kind); err != nil || id != "known" || kind != mirrorCaption {
				t.Fatalf("known mirror changed: %q, %q, %v", id, kind, err)
			}
			var count int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM published WHERE channel = ? AND mid IN ('known', 'manual')`, dst.String()).Scan(&count); err != nil || count != 2 {
				t.Fatalf("echo identities missing: %d, %v", count, err)
			}
			if err := book.deliver(t.Context(), dst, func(publish) error {
				t.Fatal("manually resolved delivery must not be replayed")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManualResolutionRefusesFinishedDelivery(t *testing.T) {
	db := testStore(t)
	src, dst := channelKey{telegramPlatform, -100}, channelKey{maxPlatform, -42}
	book := &deliveryBook{store: db, source: src, post: "message:8"}
	if err := book.set(t.Context(), dst, "done", []string{"known"}, mirrorText); err != nil {
		t.Fatal(err)
	}
	if err := db.resolveDelivery(t.Context(), src.String(), book.post, dst.String(), []string{"manual"}); err == nil {
		t.Fatal("finished delivery cannot be resolved again")
	}
	var state string
	if err := db.QueryRowContext(t.Context(), `SELECT state FROM deliveries`).Scan(&state); err != nil || state != "done" {
		t.Fatalf("finished delivery changed: %q, %v", state, err)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM published WHERE mid = 'manual'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected resolution wrote an identity: %d, %v", count, err)
	}
}
