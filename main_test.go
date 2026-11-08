package main

import (
	"slices"
	"strings"
	"testing"
	"time"
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
	alert := func(text string) error {
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
