package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestPacingSharesDestinationAcrossWorkersAndRespectsGlobalBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		byChannel := map[string][]time.Time{}
		var all []time.Time
		transport := pacedTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			key, _ := req.Context().Value(channelPacingKey{}).(string)
			mu.Lock()
			now := time.Now()
			all = append(all, now)
			byChannel[key] = append(byChannel[key], now)
			mu.Unlock()
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":{"body":{"mid":"mid.test"}}}`))}, nil
		}), 500*time.Millisecond)
		client := &maxClient{baseURL: "https://max.test", publish: &http.Client{Transport: transport}}
		var wg sync.WaitGroup
		for i := range 80 {
			wg.Go(func() {
				id := int64(i + 100)
				if i < 5 {
					id = -42
				}
				if _, err := client.send(t.Context(), id, "part", nil, true); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		sort.Slice(all, func(i, j int) bool { return all[i].Before(all[j]) })
		if len(all) != 80 {
			t.Fatalf("requests=%d", len(all))
		}
		for i := 1; i < len(all); i++ {
			if gap := all[i].Sub(all[i-1]); gap < 33*time.Millisecond {
				t.Fatalf("global gap=%s", gap)
			}
		}
		for key, times := range byChannel {
			for i := 1; i < len(times); i++ {
				if gap := times[i].Sub(times[i-1]); gap < 500*time.Millisecond {
					t.Fatalf("channel %s gap=%s", key, gap)
				}
			}
		}
	})
}

func TestPacingCancellationDoesNotSendOrCreateUncertainOutcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		transport := pacedTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`))}, nil
		}), time.Second)
		client := telegramAPI{baseURL: "https://telegram.test", token: "token", http: &http.Client{Transport: transport}}
		var result struct {
			ID int `json:"message_id"`
		}
		if err := client.call(t.Context(), "sendMessage", map[string]any{"chat_id": 42, "text": "first"}, nil, &result); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- client.call(ctx, "sendMessage", map[string]any{"chat_id": 42, "text": "second"}, nil, &result)
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, errRejected) {
			t.Fatalf("wait cancellation must be a known non-publication: %v", err)
		}
		if calls != 1 {
			t.Fatalf("sent after cancellation, calls=%d", calls)
		}
		// A cancelled waiter must not reserve the channel's next slot.
		before := time.Now()
		if err := client.call(t.Context(), "sendMessage", map[string]any{"chat_id": 42, "text": "third"}, nil, &result); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(before); elapsed != time.Second {
			t.Fatalf("cancellation consumed a future slot: %s", elapsed)
		}
	})
}

func TestMaxEditsUseDestinationPacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		transport := pacedTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"success":true}`))}, nil
		}), 500*time.Millisecond)
		client := &maxClient{baseURL: "https://max.test", publish: &http.Client{Transport: transport}}
		for i := range 3 {
			if err := client.editText(t.Context(), -42, "mid."+strconv.Itoa(i), "edit"); err != nil {
				t.Fatal(err)
			}
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("edits were not paced: %s", elapsed)
		}
	})
}
