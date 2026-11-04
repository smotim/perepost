package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-telegram/bot"
	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
)

var errRejected = errors.New("платформа отклонила публикацию")

type publishError struct {
	message string
	delay   time.Duration // positive only when the platform confirms a retry is safe
}

func (e *publishError) Error() string { return e.message }
func (e *publishError) Unwrap() error { return errRejected }

func rateLimitDelay(header string) time.Duration {
	if seconds, err := strconv.Atoi(header); err == nil && seconds > 0 {
		return time.Duration(min(seconds, 600)) * time.Second
	}

	return 2 * time.Second
}

func definitivelyRejected(status int) bool {
	return status >= http.StatusBadRequest && status < http.StatusInternalServerError && status != http.StatusRequestTimeout
}

// retryPublish repeats only explicit rejections. A lost response or a 5xx
// might follow a successful publication and must go to the delivery journal.
func retryPublish(ctx context.Context, fn func() ([]string, error)) ([]string, error) {
	for attempt := 1; ; attempt++ {
		ids, err := fn()
		known, ok := errors.AsType[*publishError](err)
		if !ok || known.delay <= 0 || attempt == 5 {
			return ids, err
		}
		select {
		case <-ctx.Done():
			return ids, err
		case <-time.After(max(known.delay, time.Duration(1<<attempt)*time.Second)):
		}
	}
}

// retry repeats transient failures — network, 429, 5xx, "attachment not
// ready" — with a growing pause: 2, 4, 8, 16 seconds.
func retry(ctx context.Context, fn func() error) error {
	const attempts = 5
	delay := 2 * time.Second
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || attempt == attempts || permanent(err) {
			return err
		}
		slog.Warn("повтор после ошибки", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// permanent reports errors that another attempt will not fix.
func permanent(err error) bool {
	if rejection, ok := errors.AsType[*publishError](err); ok {
		return rejection.delay == 0
	}
	if maxErr, ok := errors.AsType[*maxbot.Error](err); ok {
		return !maxErr.IsAttachmentNotReady()
	}

	return errors.Is(err, errTooBig) || errors.Is(err, errNotOnVolume) || errors.Is(err, bot.ErrorBadRequest) || errors.Is(err, bot.ErrorForbidden) ||
		errors.Is(err, bot.ErrorUnauthorized) || errors.Is(err, bot.ErrorNotFound) || errors.Is(err, errMaxMediaExpired) || errors.Is(err, errMaxMediaSize)
}
