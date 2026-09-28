package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// healthInterval is how often to check that the bot can do its job.
	healthInterval = 5 * time.Minute
	// checkParallelism and checkTimeout keep a check of hundreds of channels
	// short without hitting the platforms' rate limits.
	checkParallelism = 8
	checkTimeout     = 30 * time.Second
)

// monitor checks that both platforms answer and the bot is still an admin of
// every channel. A passed check POSTs a heartbeat to HEARTBEAT_URL: when the
// heartbeats stop, the bot is down and the monitoring service raises an alarm.
// The admin is told only when the state changes, not every five minutes.
type monitor struct {
	relay     *relay
	sources   []*source
	to        []destination // every destination once
	heartbeat string
	client    *http.Client
	failing   bool
}

func (m *monitor) run(ctx context.Context) {
	for {
		m.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(healthInterval):
		}
	}
}

func (m *monitor) check(ctx context.Context) {
	checks := make([]func(context.Context) error, 0, len(m.sources)+len(m.to))
	for _, src := range m.sources {
		checks = append(checks, func(ctx context.Context) error { return checkTelegramAdmin(ctx, m.relay.tg, src.channel) })
	}
	for _, dst := range m.to {
		checks = append(checks, dst.checkAdmin)
	}
	problems := runChecks(ctx, checks, checkParallelism, checkTimeout)
	if ctx.Err() != nil {
		return // shutting down, not failing
	}

	switch {
	case len(problems) > 0:
		slog.Error("проверка не прошла", "problems", problems)
		if !m.failing {
			_ = m.relay.alert("проверка не прошла, посты могут не пересылаться:\n— " + strings.Join(problems, "\n— "))
		}
		m.failing = true

		return
	case m.failing:
		slog.Info("проверка снова проходит")
		_ = m.relay.alert("проверка снова проходит, посты пересылаются")
		m.failing = false
	}

	if m.heartbeat != "" {
		m.ping(ctx)
	}
}

// runChecks runs the checks a few at a time, each with its own timeout, and
// returns the failures in the order of the checks.
func runChecks(ctx context.Context, checks []func(context.Context) error, parallel int, timeout time.Duration) []string {
	errs := make([]error, len(checks))
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, check := range checks {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()

			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			errs[i] = check(ctx)
		})
	}
	wg.Wait()

	var problems []string
	for _, err := range errs {
		if err != nil {
			problems = append(problems, err.Error())
		}
	}

	return problems
}

// ping is a POST without a body: both GlitchTip and healthchecks.io accept it.
func (m *monitor) ping(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.heartbeat, nil)
	if err != nil {
		slog.Error("HEARTBEAT_URL не разобрать", "err", err)

		return
	}
	resp, err := m.client.Do(req)
	if err != nil {
		slog.Warn("пульс не отправлен", "err", withoutURL(err))

		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("пульс не принят", "status", resp.Status)
	}
}
