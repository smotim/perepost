package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newMonitor(f *fake) *monitor {
	return &monitor{relay: f.relay, sources: []*source{f.source}, to: f.source.to}
}

func TestMonitorPingsAndReportsOnlyStateChanges(t *testing.T) {
	f := fakes(t)
	var pings atomic.Int32
	heartbeat := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("heartbeat method %s", r.Method)
		}
		pings.Add(1)
	}))
	t.Cleanup(heartbeat.Close)
	m := newMonitor(f)
	m.heartbeat, m.client = heartbeat.URL, heartbeat.Client()
	ctx := context.Background()

	m.check(ctx)
	if pings.Load() != 1 || len(f.alerted()) != 0 {
		t.Fatalf("healthy: pings %d, alerts %q", pings.Load(), f.alerted())
	}

	// The bot lost its admin rights in MAX: one message, no heartbeat
	f.maxAdmin.Store(false)
	m.check(ctx)
	m.check(ctx)
	if alerts := f.alerted(); pings.Load() != 1 || len(alerts) != 1 || !strings.Contains(alerts[0], "не администратор канала MAX") {
		t.Fatalf("failing: pings %d, alerts %q", pings.Load(), alerts)
	}

	f.maxAdmin.Store(true)
	m.check(ctx)
	if alerts := f.alerted(); pings.Load() != 2 || len(alerts) != 2 || !strings.Contains(alerts[1], "снова проходит") {
		t.Fatalf("recovered: pings %d, alerts %q", pings.Load(), alerts)
	}
}

func TestMonitorNoticesRemovedTelegramAdmin(t *testing.T) {
	f := fakes(t)
	f.tgAdmin.Store(false)
	newMonitor(f).check(context.Background())

	if alerts := f.alerted(); len(alerts) != 1 || !strings.Contains(alerts[0], "не администратор канала Telegram «Канал»") {
		t.Fatalf("alerts %q", alerts)
	}
}

func TestChecksRunInParallelWithOwnTimeouts(t *testing.T) {
	var running, peak atomic.Int32
	checks := make([]func(context.Context) error, 20)
	for i := range checks {
		checks[i] = func(ctx context.Context) error {
			n := running.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			defer running.Add(-1)
			if i%5 == 0 { // hangs: only its own timeout stops it
				<-ctx.Done()

				return fmt.Errorf("check %d: %w", i, ctx.Err())
			}
			time.Sleep(10 * time.Millisecond)

			return nil
		}
	}

	problems := runChecks(context.Background(), checks, 4, 100*time.Millisecond)
	want := []string{"check 0", "check 5", "check 10", "check 15"}
	if len(problems) != len(want) {
		t.Fatalf("problems %q", problems)
	}
	for i, p := range problems {
		if !strings.HasPrefix(p, want[i]) {
			t.Fatalf("problems %q, want in order %q", problems, want)
		}
	}
	if n := peak.Load(); n > 4 || n < 2 {
		t.Fatalf("peak parallelism %d", n)
	}
}
