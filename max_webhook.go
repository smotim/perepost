package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

const (
	maxEvent  = "message_created"
	maxEdit   = "message_edited"
	maxDelete = "message_removed"
)

type maxMessage struct {
	Recipient model.Recipient `json:"recipient"`
	Body      maxMessageBody  `json:"body"`
	Link      *struct {
		Type    string         `json:"type"`
		Message maxMessageBody `json:"message"`
	} `json:"link"`
	URL string `json:"url"`
}

type maxMessageBody struct {
	Mid         string      `json:"mid"`
	Text        string      `json:"text"`
	Attachments []maxMedia  `json:"attachments"`
	Markup      []maxMarkup `json:"markup"`
}

type maxMedia struct {
	model.Attachment
	Payload struct {
		model.Payload
		FileName string `json:"filename"`
		Size     int64  `json:"size"`
	} `json:"payload"`
}

type maxMarkup struct {
	Type     string `json:"type"`
	From     int    `json:"from"`
	Length   int    `json:"length"`
	URL      string `json:"url"`
	UserLink string `json:"user_link"`
}

var errInvalidUpdate = errors.New("некорректное событие вебхука")

func receiveWebhook(header, secret string, accept func(context.Context, []byte) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)

			return
		}
		if subtle.ConstantTimeCompare([]byte(req.Header.Get(header)), []byte(secret)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}
		payload, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 2<<20))
		if err != nil {
			if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			} else {
				w.WriteHeader(http.StatusBadRequest)
			}

			return
		}
		if err := accept(req.Context(), payload); err != nil {
			if errors.Is(err, errInvalidUpdate) {
				w.WriteHeader(http.StatusBadRequest)
			} else {
				slog.Error("не удалось записать событие", "err", err)
				w.WriteHeader(http.StatusServiceUnavailable)
			}

			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func maxWebhook(secret string, db *store, sources map[int64]*maxSource) http.Handler {
	return receiveWebhook("X-Max-Bot-Api-Secret", secret, func(ctx context.Context, payload []byte) error {
		var update struct {
			Type      string          `json:"update_type"`
			Message   json.RawMessage `json:"message"`
			Timestamp int64           `json:"timestamp"`
			ChatID    int64           `json:"chat_id"`
			MessageID string          `json:"message_id"`
		}
		if err := json.Unmarshal(payload, &update); err != nil {
			slog.Warn("не удалось разобрать событие MAX", "type", update.Type, "err", err)

			return errInvalidUpdate
		}
		if update.Type == maxDelete {
			if src := sources[update.ChatID]; src != nil && src.hasChange(maxDelete) && update.MessageID != "" {
				return db.enqueueChange(ctx, channelKey{maxPlatform, update.ChatID}, change{Kind: maxDelete, Mid: update.MessageID, At: update.Timestamp})
			}

			return nil
		}
		if update.Type != maxEvent && update.Type != maxEdit {
			return nil
		}
		var head struct {
			Recipient struct {
				ChatID   int64           `json:"chat_id"`
				ChatType model.ChatType  `json:"chat_type"`
				PostID   json.RawMessage `json:"post_id"`
			} `json:"recipient"`
			Body struct{ Mid string } `json:"body"`
		}
		if err := json.Unmarshal(update.Message, &head); err != nil {
			slog.Warn("не удалось разобрать заголовок поста MAX", "type", update.Type, "err", err)

			return errInvalidUpdate
		}
		id := head.Recipient.ChatID
		src := sources[id]
		if src == nil || head.Recipient.ChatType != model.ChatTypeChannel {
			return nil
		}
		mid := head.Body.Mid
		if mid == "" && len(head.Recipient.PostID) > 0 {
			if err := json.Unmarshal(head.Recipient.PostID, &mid); err != nil {
				var number json.Number
				if err := json.Unmarshal(head.Recipient.PostID, &number); err != nil {
					slog.Warn("не удалось разобрать идентификатор поста MAX", "type", update.Type, "err", err)

					return errInvalidUpdate
				}
				mid = number.String()
			}
		}
		if mid == "" {
			slog.Warn("MAX прислал пост без идентификатора", "chat_id", id)

			return nil
		}
		key := channelKey{maxPlatform, id}
		if update.Type == maxEdit {
			if !src.hasChange(maxEdit) {
				return nil
			}

			return db.enqueueChange(ctx, key, change{Kind: maxEdit, Mid: mid, At: update.Timestamp, Message: update.Message})
		}

		return db.enqueue(ctx, key, mid, "message:"+mid, update.Message, time.Now())
	})
}

func (m *maxClient) webhook(ctx context.Context, address, secret string, events []string, register bool) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	existing, err := m.api.Subscriptions.GetSubscriptions(ctx)
	if err != nil {
		return fmt.Errorf("не удалось проверить вебхук MAX: %w", err)
	}
	var ours bool
	var subscriptionVersion string
	for _, sub := range existing.Subscriptions {
		if sub.URL == address {
			if hasEvents(sub.UpdateTypes, events) {
				return nil
			}
			ours = true
			subscriptionVersion = sub.Version
			events = unionEvents(sub.UpdateTypes, events)
			break
		}
	}
	if !register || (!ours && len(existing.Subscriptions) > 0) {
		return errors.New("подписка MAX по MAX_WEBHOOK_URL не найдена или в ней отсутствуют нужные события")
	}
	result, err := m.api.Subscriptions.Subscribe(ctx, address, secret, events, subscriptionVersion)
	if err != nil {
		return fmt.Errorf("не удалось подключить вебхук MAX: %w", err)
	}
	if !result.Success {
		return errors.New("MAX отклонил регистрацию вебхука")
	}

	return nil
}

func hasEvents(current, required []string) bool {
	return !slices.ContainsFunc(required, func(event string) bool { return !slices.Contains(current, event) })
}

func unionEvents(current, required []string) []string {
	result := slices.Clone(current)
	for _, event := range required {
		if !slices.Contains(result, event) {
			result = append(result, event)
		}
	}

	return result
}
