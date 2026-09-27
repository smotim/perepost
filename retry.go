package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/go-telegram/bot"
	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
)

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
	if maxErr, ok := errors.AsType[*maxbot.Error](err); ok {
		return !maxErr.IsAttachmentNotReady()
	}

	return errors.Is(err, errTooBig) || errors.Is(err, bot.ErrorBadRequest) || errors.Is(err, bot.ErrorForbidden) ||
		errors.Is(err, bot.ErrorUnauthorized) || errors.Is(err, bot.ErrorNotFound)
}
