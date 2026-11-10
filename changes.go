package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-telegram/bot/models"
	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

type syncOptions struct{ edits, deletes bool }

var errMirrorMissing = errors.New("копия сообщения отсутствует")

func addSyncOptions(changes map[int64]syncOptions, id int64, route route) {
	old := changes[id]
	changes[id] = syncOptions{edits: old.edits || route.syncEdits, deletes: old.deletes || route.syncDeletes}
}

func (s *source) hasEdits() bool {
	for _, options := range s.changes {
		if options.edits {
			return true
		}
	}

	return false
}

func (s *maxSource) hasChange(kind string) bool {
	for _, options := range s.changes {
		if kind == maxEdit && options.edits || kind == maxDelete && options.deletes {
			return true
		}
	}

	return false
}

func telegramPostText(msgs []*models.Message) (richText, bool) {
	var text richText
	preview := true
	if from, ok := forwardedFrom(msgs[0]); ok {
		text.append(from)
	}
	for _, m := range msgs {
		if m.Text != "" {
			text.append(newRichText(m.Text, m.Entities))
		} else {
			text.append(newRichText(m.Caption, m.CaptionEntities))
		}
		if o := m.LinkPreviewOptions; o != nil && o.IsDisabled != nil && *o.IsDisabled {
			preview = false
		}
	}

	return text, preview
}

func maxPostText(message maxMessage) richText {
	text := message.Body.richText()
	if message.Link != nil && message.Link.Type == "forward" {
		text.append(message.Link.Message.richText())
	}

	return text
}

func telegramMessages(payloads [][]byte) ([]*models.Message, error) {
	var messages []*models.Message
	var problems []error
	for _, payload := range payloads {
		var m models.Message
		if err := json.Unmarshal(payload, &m); err != nil {
			problems = append(problems, err)
			if _, partial := errors.AsType[*json.UnmarshalTypeError](err); !partial {
				continue
			}
		}
		messages = append(messages, &m)
	}

	return messages, errors.Join(problems...)
}

func (b *deliveryBook) startChange(ctx context.Context, p pendingPost) (change, bool) {
	var event change
	if len(p.payloads) == 0 {
		return event, false
	}
	if err := json.Unmarshal(p.payloads[0], &event); err != nil {
		slog.Error("не удалось разобрать правку или удаление", "err", err)

		return event, false
	}
	b.root = "message:" + event.Mid
	if event.Group != "" {
		b.root = event.Group
	}
	var echo bool
	b.err = b.store.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM published WHERE channel = ? AND mid = ?)`,
		b.source.String(), event.Mid).Scan(&echo)
	if b.err != nil || echo {
		return event, false
	}
	ok, err := b.store.acceptRevision(ctx, b.source, event)
	b.err = err

	return event, ok && err == nil
}

func (r *relay) editFromTelegram(ctx context.Context, src *source, p pendingPost, book *deliveryBook) {
	event, ok := book.startChange(ctx, p)
	if !ok || event.Kind != telegramEdit {
		return
	}
	payloads := [][]byte{event.Message}
	if strings.HasPrefix(event.Group, "album:") {
		if _, err := telegramMessages(payloads); err != nil {
			r.changeError(ctx, book, "", err)

			return
		}
		book.root, payloads, book.err = book.store.reviseAlbum(ctx, book.source, event)
		if book.err != nil {
			return
		}
	}
	messages, err := telegramMessages(payloads)
	if err != nil {
		r.changeError(ctx, book, "", err)

		return
	}
	if len(messages) == 0 {
		return
	}
	text, preview := telegramPostText(messages)
	if src.channel.Username != "" && slices.ContainsFunc(messages, unsupported) {
		text.append(linkText("Открыть пост в Telegram", src.postRef(messages[0].ID)))
	}
	for _, dst := range src.to {
		if !src.changes[dst.id].edits || book.err != nil {
			continue
		}
		key := channelKey{maxPlatform, dst.id}
		err := syncMirrorText(ctx, book, key, func(withCaption bool) (string, []string) {
			parts := text.split(maxTextLimit)
			if withCaption && len(parts) > 0 {
				return parts[0], parts[1:]
			}

			return "", parts
		}, func(part mirrorPart, value string) error {
			return r.max.editText(ctx, dst.id, part.id, value)
		}, func(part mirrorPart) error {
			return r.max.deleteMirror(ctx, dst.id, part.id)
		}, func(part string, publish publish) error {
			return publish(ctx, func() ([]string, error) {
				return retryPublish(ctx, func() ([]string, error) { return r.max.send(ctx, dst.id, part, nil, preview) })
			}, mirrorText)
		})
		r.changeError(ctx, book, dst.title, err)
	}
}

func (r *relay) changeFromMax(ctx context.Context, src *maxSource, p pendingPost, book *deliveryBook) {
	event, ok := book.startChange(ctx, p)
	if !ok {
		return
	}
	var text richText
	if event.Kind == maxEdit {
		var m maxMessage
		if err := json.Unmarshal(event.Message, &m); err != nil {
			r.changeError(ctx, book, "", err)

			return
		}
		text = maxPostText(m)
		if m.Link != nil && m.Link.Type == "forward" && webLink(m.URL) {
			text.append(linkText("Открыть пост в MAX", m.URL))
		}
	}
	for _, dst := range src.to {
		if book.err != nil {
			return
		}
		key := channelKey{telegramPlatform, dst.ID}
		remove := func(part mirrorPart) error { return r.tgAPI.deleteMirror(ctx, dst.ID, part) }
		var err error
		switch {
		case event.Kind == maxDelete && src.changes[dst.ID].deletes:
			err = removeMirrors(ctx, book, key, remove)
		case event.Kind == maxEdit && src.changes[dst.ID].edits:
			err = syncMirrorText(ctx, book, key, func(withCaption bool) (telegramText, []telegramText) {
				var caption telegramText
				from := 0
				if withCaption {
					caption, from = text.telegramPart(from, 1024)
				}
				var parts []telegramText
				for from < len(text.units) {
					part, next := text.telegramPart(from, 4096)
					if part.text != "" {
						parts = append(parts, part)
					}
					from = next
				}

				return caption, parts
			}, func(part mirrorPart, value telegramText) error {
				return r.tgAPI.editText(ctx, dst.ID, part, value)
			}, remove, func(part telegramText, publish publish) error {
				return r.sendTelegram(ctx, dst.ID, part, nil, true, publish)
			})
		}
		r.changeError(ctx, book, dst.Title, err)
	}
}

func (r *relay) changeError(ctx context.Context, book *deliveryBook, destination string, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	slog.Warn("не удалось синхронизировать пост", "source", book.source, "post", book.root, "to", destination, "err", err)
	_ = r.alert(ctx, fmt.Sprintf("не удалось синхронизировать пост %s %s → «%s»: %v", book.source, book.root, destination, err))
}

func syncMirrorText[T any](ctx context.Context, book *deliveryBook, dst channelKey, render func(bool) (T, []T),
	edit func(mirrorPart, T) error, remove func(mirrorPart) error, appendPart func(T, publish) error) error {
	parts, err := book.mirrors(ctx, dst)
	if err != nil {
		book.err = err

		return err
	}
	if len(parts) == 0 {
		return nil // no known copy: do not turn a historical edit into a new post
	}
	captionAt := slices.IndexFunc(parts, func(p mirrorPart) bool { return p.kind == mirrorCaption })
	caption, desired := render(captionAt >= 0)
	if captionAt >= 0 {
		if err := edit(parts[captionAt], caption); err != nil {
			if errors.Is(err, errMirrorMissing) {
				return nil
			}
			return err
		}
	}
	existing := slices.DeleteFunc(parts, func(p mirrorPart) bool { return p.kind != mirrorText })
	for i := range min(len(existing), len(desired)) {
		if err := edit(existing[i], desired[i]); err != nil {
			if errors.Is(err, errMirrorMissing) {
				// Keep its position in the saved copy and skip this destination.
				// Forgetting it or appending a longer revision would resurrect
				// a post (or part) the channel's administrator removed.
				return nil
			}
			return err
		}
	}
	if len(existing) > len(desired) {
		return deleteMirrorParts(ctx, book, dst, existing[len(desired):], remove)
	}
	if len(existing) < len(desired) {
		if err := book.store.checkUncertain(ctx, dst); err != nil {
			return err
		}
		return book.deliver(ctx, dst, func(publish publish) error {
			for _, part := range desired[len(existing):] {
				if err := appendPart(part, publish); err != nil {
					return err
				}
			}

			return nil
		})
	}

	return nil
}

func removeMirrors(ctx context.Context, book *deliveryBook, dst channelKey, remove func(mirrorPart) error) error {
	parts, err := book.mirrors(ctx, dst)
	if err != nil {
		book.err = err

		return err
	}

	return deleteMirrorParts(ctx, book, dst, parts, remove)
}

func deleteMirrorParts(ctx context.Context, book *deliveryBook, dst channelKey, parts []mirrorPart, remove func(mirrorPart) error) error {
	var problems []error
	for _, part := range parts {
		if err := remove(part); err != nil {
			problems = append(problems, err)

			continue
		}
		if err := book.forgetMirror(ctx, dst, part); err != nil {
			book.err = err

			return err
		}
	}

	return errors.Join(problems...)
}

func (t telegramAPI) editText(ctx context.Context, chatID int64, part mirrorPart, text telegramText) error {
	method, textKey, entityKey := "editMessageText", "text", "entities"
	if part.kind == mirrorCaption {
		method, textKey, entityKey = "editMessageCaption", "caption", "caption_entities"
	}
	mid, err := strconv.Atoi(part.id)
	if err != nil {
		return err
	}
	return retry(ctx, func() error {
		var result json.RawMessage
		params := map[string]any{"chat_id": chatID, "message_id": mid, textKey: text.text}
		if len(text.entities) > 0 {
			params[entityKey] = text.entities
		}
		err := t.call(ctx, method, params, nil, &result)
		if errors.Is(err, errRejected) && strings.Contains(err.Error(), "message is not modified") {
			return nil
		}
		if errors.Is(err, errRejected) && strings.Contains(err.Error(), "message to edit not found") {
			slog.Info("копия Telegram отсутствует, правка пропущена", "chat_id", chatID, "message_id", mid)
			return fmt.Errorf("%w: %w", errMirrorMissing, err)
		}

		return err
	})
}

func (t telegramAPI) deleteMirror(ctx context.Context, chatID int64, part mirrorPart) error {
	mid, err := strconv.Atoi(part.id)
	if err != nil {
		return err
	}
	return retry(ctx, func() error {
		var result bool
		err := t.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": mid}, nil, &result)
		if errors.Is(err, errRejected) && strings.Contains(err.Error(), "message to delete not found") {
			return nil
		}
		if errors.Is(err, errRejected) && strings.Contains(err.Error(), "message can't be deleted") {
			// Telegram cannot remove copies older than 48 hours. Its own
			// channel posts can still be edited; remove their stale contents.
			if part.kind == mirrorMedia {
				part.kind = mirrorCaption
			}
			err := t.editText(ctx, chatID, part, telegramText{text: "Содержимое удалено."})
			if errors.Is(err, errMirrorMissing) {
				return nil
			}
			return err
		}
		if err == nil && !result {
			return &publishError{message: "Telegram не подтвердил удаление"}
		}

		return err
	})
}

func (m *maxClient) editText(ctx context.Context, chatID int64, mid, text string) error {
	payload, err := json.Marshal(struct {
		Text   string           `json:"text"`
		Format model.TextFormat `json:"format"`
	}{text, model.FormatHTML})
	if err != nil {
		return err
	}
	return retry(ctx, func() error { return m.mutateMessage(ctx, http.MethodPut, chatID, mid, payload) })
}

func (m *maxClient) deleteMirror(ctx context.Context, chatID int64, mid string) error {
	return retry(ctx, func() error { return m.mutateMessage(ctx, http.MethodDelete, chatID, mid, nil) })
}

func (m *maxClient) mutateMessage(ctx context.Context, method string, chatID int64, mid string, payload []byte) error {
	ctx = withPacingChannel(ctx, strconv.FormatInt(chatID, 10))
	query := url.Values{"message_id": {mid}}
	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+"/messages?"+query.Encode(), bytes.NewReader(payload))
	if err != nil {
		return &publishError{message: err.Error()}
	}
	req.Header.Set(maxbot.AuthorizationHeader, m.token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.publish.Do(req)
	if err != nil {
		return withoutURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if method == http.MethodDelete && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 300 {
		var failure maxbot.Error
		_ = json.NewDecoder(resp.Body).Decode(&failure)
		message := fmt.Sprintf("MAX ответил %s: %v", resp.Status, failure)
		if resp.StatusCode == http.StatusTooManyRequests {
			return &publishError{message, rateLimitDelay(resp.Header.Get("Retry-After"))}
		}
		if definitivelyRejected(resp.StatusCode) {
			return &publishError{message: message}
		}

		return errors.New(message)
	}
	var result model.SimpleQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.Success {
		return &publishError{message: "MAX не подтвердил изменение"}
	}

	return nil
}
