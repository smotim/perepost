package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestPollingProgressRequiresSuccessfulResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		progress   bool
	}{
		{"empty batch", `{"ok":true,"result":[]}`, http.StatusOK, true},
		{"conflicting poller", `{"ok":false,"error_code":409,"description":"another getUpdates request"}`, http.StatusConflict, false},
		{"malformed response", `{"ok":true,"result":`, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var lastPoll atomic.Int64
				initial := time.Now().Add(-time.Hour).UnixNano()
				lastPoll.Store(initial)
				calls := 0
				client := telegramAPI{baseURL: "https://telegram.test", token: "token", http: &http.Client{
					Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						_ = req.Body.Close()
						calls++
						if calls == 2 {
							cancel()

							return nil, ctx.Err()
						}

						return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
					}),
				}}
				if err := client.poll(ctx, nil, nil, []string{"channel_post"}, &lastPoll); err != nil {
					t.Fatal(err)
				}
				if calls != 2 {
					t.Fatalf("poll did not continue: calls=%d", calls)
				}
				if advanced := lastPoll.Load() > initial; advanced != tc.progress {
					t.Fatalf("poll progress=%t, want %t", advanced, tc.progress)
				}
			})
		})
	}
}
