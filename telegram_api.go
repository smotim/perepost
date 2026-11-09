package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-telegram/bot/models"
)

type telegramAPI struct {
	baseURL string
	token   string
	http    *http.Client
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Only two calls need lower-level access: getUpdates acknowledges an offset
// before the SDK handler runs, and SDK uploads buffer the entire multipart body.
func (t telegramAPI) call(ctx context.Context, method string, payload any, files []maxFile, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return &publishError{message: err.Error()}
	}
	if method != "getUpdates" {
		var destination struct {
			ChatID json.RawMessage `json:"chat_id"`
		}
		if err := json.Unmarshal(data, &destination); err != nil {
			return &publishError{message: err.Error()}
		}
		if len(destination.ChatID) > 0 {
			ctx = withPacingChannel(ctx, string(destination.ChatID))
		}
	}
	var body io.Reader = bytes.NewReader(data)
	contentType, length := "application/json", int64(len(data))
	if len(files) > 0 {
		form, err := telegramMultipart(data, files)
		if err != nil {
			return &publishError{message: err.Error()}
		}
		defer form.close()
		body, contentType, length = form.body, form.contentType, form.length
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/bot"+t.token+"/"+method, body)
	if err != nil {
		return &publishError{message: "не удалось собрать запрос к Telegram"}
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = length
	resp, err := t.http.Do(req)
	if err != nil {
		return withoutURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var reply struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&reply); err != nil {
		return fmt.Errorf("ответ Telegram %s: %w", resp.Status, err)
	}
	if !reply.OK || resp.StatusCode >= 300 {
		message := fmt.Sprintf("Telegram ответил %s: %s", resp.Status, reply.Description)
		switch {
		case reply.ErrorCode == http.StatusTooManyRequests && resp.StatusCode < 500:
			return &publishError{message, rateLimitDelay(strconv.Itoa(reply.Parameters.RetryAfter))}
		case definitivelyRejected(reply.ErrorCode) && resp.StatusCode < 500:
			return &publishError{message: message}
		default:
			return errors.New(message)
		}
	}

	return json.Unmarshal(reply.Result, result)
}

type multipartBody struct {
	body        io.Reader
	contentType string
	length      int64
	files       []*os.File
}

func (m *multipartBody) close() {
	for _, f := range m.files {
		_ = f.Close()
	}
}

func telegramMultipart(payload []byte, files []maxFile) (*multipartBody, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	var header bytes.Buffer
	form := multipart.NewWriter(&header)
	body := &multipartBody{contentType: form.FormDataContentType()}
	for key, field := range fields {
		value := string(field)
		if len(field) > 0 && field[0] == '"' {
			if err := json.Unmarshal(field, &value); err != nil {
				return nil, err
			}
		}
		if err := form.WriteField(key, value); err != nil {
			return nil, err
		}
	}
	var readers []io.Reader
	appendHeader := func() {
		readers = append(readers, bytes.NewReader(bytes.Clone(header.Bytes())))
		body.length += int64(header.Len())
		header.Reset()
	}
	for i, media := range files {
		if _, err := form.CreateFormFile(fileField(i), media.name); err != nil {
			body.close()

			return nil, err
		}
		f, err := os.Open(media.path)
		if err != nil {
			body.close()

			return nil, withoutPath(err)
		}
		appendHeader()
		body.files = append(body.files, f)
		readers = append(readers, io.LimitReader(f, media.size))
		body.length += media.size
	}
	if err := form.Close(); err != nil {
		body.close()

		return nil, err
	}
	appendHeader()
	body.body = io.MultiReader(readers...)

	return body, nil
}

func fileField(i int) string { return "file" + strconv.Itoa(i) }

const telegramEdit = "edited_channel_post"

type telegramUpdate struct {
	ID   int64           `json:"update_id"`
	Post json.RawMessage `json:"channel_post"`
	Edit json.RawMessage `json:"edited_channel_post"`
}

func acceptTelegram(ctx context.Context, db *store, sources map[int64]*source, update telegramUpdate) error {
	var head struct {
		ID       int         `json:"message_id"`
		Chat     models.Chat `json:"chat"`
		GroupID  string      `json:"media_group_id"`
		EditDate int64       `json:"edit_date"`
	}
	payload := update.Post
	if len(update.Edit) != 0 {
		payload = update.Edit
	}
	if len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, &head); err != nil || head.ID == 0 {
		return errInvalidUpdate
	}
	src := sources[head.Chat.ID]
	if src == nil {
		return nil
	}
	mid := strconv.Itoa(head.ID)
	key := channelKey{telegramPlatform, head.Chat.ID}
	group := "message:" + mid
	if head.GroupID != "" {
		group = "album:" + head.GroupID
	}
	if len(update.Edit) != 0 {
		if !src.hasEdits() {
			return nil
		}

		return db.enqueueChange(ctx, key, change{Kind: telegramEdit, Mid: mid, Group: group,
			At: head.EditDate, Sequence: update.ID, Message: payload})
	}

	return db.enqueue(ctx, key, mid, group, payload, time.Now())
}

func telegramWebhook(secret string, db *store, sources map[int64]*source) http.Handler {
	return receiveWebhook("X-Telegram-Bot-Api-Secret-Token", secret, func(ctx context.Context, payload []byte) error {
		var update telegramUpdate
		if err := json.Unmarshal(payload, &update); err != nil {
			return errInvalidUpdate
		}

		return acceptTelegram(ctx, db, sources, update)
	})
}

func (t telegramAPI) poll(ctx context.Context, db *store, sources map[int64]*source, events []string) error {
	var offset int64
	for ctx.Err() == nil {
		var updates []telegramUpdate
		request, cancel := context.WithTimeout(ctx, 40*time.Second)
		err := t.call(request, "getUpdates", map[string]any{"offset": offset, "timeout": 30, "allowed_updates": events}, nil, &updates)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			slog.Warn("не удалось получить посты Telegram", "err", err)
			delay := 2 * time.Second
			if rejected, ok := errors.AsType[*publishError](err); ok {
				delay = max(delay, rejected.delay)
			}
			select {
			case <-ctx.Done():
			case <-time.After(delay):
			}

			continue
		}
		if len(updates) == 0 {
			// The last request already acknowledged the previous batch.
			// After a week of inactivity Telegram may pick a new random ID.
			offset = 0
		}
		for _, update := range updates {
			if err := acceptTelegram(ctx, db, sources, update); errors.Is(err, errInvalidUpdate) {
				slog.Warn("не удалось разобрать событие Telegram", "update", update.ID)
			} else if err != nil {
				return err
			}
			// Telegram confirms a batch only on the next request with this offset.
			offset = update.ID + 1
		}
	}

	return nil
}
