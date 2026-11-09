package main

import (
	"context"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type channelPacingKey struct{}

// The destination is request metadata, also for edits addressed by message ID.
func withPacingChannel(ctx context.Context, channel string) context.Context {
	return context.WithValue(ctx, channelPacingKey{}, channel)
}

type rateTransport struct {
	next     http.RoundTripper
	global   *rate.Limiter
	interval time.Duration
	mu       sync.Mutex
	channels map[string]*rate.Limiter
}

func (t *rateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	channel, _ := req.Context().Value(channelPacingKey{}).(string)
	if err := t.wait(req.Context(), channel); err != nil {
		return nil, &publishError{message: err.Error()}
	}
	return t.next.RoundTrip(req)
}

// Acquire both budgets together, immediately before sending. Waiting for two
// independent reservations in sequence could bunch a channel's requests after
// a busy global queue. Cancel tentative reservations before waiting instead.
func (t *rateTransport) wait(ctx context.Context, key string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		t.mu.Lock()
		now := time.Now()
		global := t.global.ReserveN(now, 1)
		delay := global.DelayFrom(now)
		var channel *rate.Reservation
		if key != "" {
			limiter := t.channels[key]
			if limiter == nil {
				limiter = rate.NewLimiter(rate.Every(t.interval), 1)
				t.channels[key] = limiter
			}
			channel = limiter.ReserveN(now, 1)
			delay = max(delay, channel.DelayFrom(now))
		}
		if delay == 0 {
			t.mu.Unlock()
			return nil
		}
		global.CancelAt(now)
		if channel != nil {
			channel.CancelAt(now)
		}
		t.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func pacedTransport(next http.RoundTripper, interval time.Duration) http.RoundTripper {
	return &rateTransport{next: next, global: rate.NewLimiter(30, 1), interval: interval, channels: make(map[string]*rate.Limiter)}
}
