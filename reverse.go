package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/go-telegram/bot/models"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

type maxSource struct {
	channel maxChat
	to      []*models.ChatFullInfo
	changes map[int64]syncOptions
}

type maxFile struct {
	kind     model.AttachmentType
	path     string
	name     string
	size     int64
	sourceID string
}

var maxEntityTypes = map[string]models.MessageEntityType{
	"strong":        models.MessageEntityTypeBold,
	"emphasized":    models.MessageEntityTypeItalic,
	"underline":     models.MessageEntityTypeUnderline,
	"strikethrough": models.MessageEntityTypeStrikethrough,
	"monospaced":    models.MessageEntityTypeCode,
	"quote":         models.MessageEntityTypeBlockquote,
	"link":          models.MessageEntityTypeTextLink,
	"heading":       models.MessageEntityTypeBold,
	"highlighted":   models.MessageEntityTypeUnderline,
	"user_mention":  models.MessageEntityTypeTextLink,
}

func (b maxMessageBody) richText() richText {
	text := newRichText(b.Text, nil)
	for _, span := range b.Markup {
		if span.Type == "user_mention" {
			span.URL = ""
			username := strings.TrimPrefix(span.UserLink, "@")
			if validMaxUsername.MatchString(username) {
				span.URL = "https://max.ru/" + username
			}
		}
		kind, ok := maxEntityTypes[span.Type]
		if !ok || span.From < 0 || span.From >= len(text.units) || span.Length <= 0 || span.Length > len(text.units)-span.From {
			continue
		}
		if kind == models.MessageEntityTypeTextLink && !webLink(span.URL) {
			continue
		}
		end := span.From + span.Length
		if lowSurrogate(text.units[span.From]) || (end < len(text.units) && lowSurrogate(text.units[end])) {
			continue
		}
		text.entities = append(text.entities, models.MessageEntity{Type: kind, Offset: span.From, Length: span.Length, URL: span.URL})
	}

	return text
}

func lowSurrogate(unit uint16) bool { return unit >= 0xDC00 && unit <= 0xDFFF }

func webLink(value string) bool {
	u, err := url.Parse(value)

	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil
}

type telegramText struct {
	text     string
	entities []models.MessageEntity
}

func (t richText) telegramPart(from, limit int) (telegramText, int) {
	from = skipSpace(t.units, from)
	end := trimRight(t.units, len(t.units))
	if from >= end {
		return telegramText{}, len(t.units)
	}
	to := min(from+limit, end)
	if to < end {
		start := from + limit/2
		for _, sep := range []uint16{'\n', ' '} {
			for at, unit := range slices.Backward(t.units[start:to]) {
				if unit == sep {
					to = start + at

					break
				}
			}
			if to < from+limit {
				break
			}
		}
		if lowSurrogate(t.units[to]) {
			to--
		}
	}
	next := to
	to = trimRight(t.units, to)
	part := telegramText{text: string(utf16.Decode(t.units[from:to]))}
	for _, e := range t.entities {
		left, right := max(e.Offset, from), min(e.Offset+e.Length, to)
		if left < right {
			e.Offset, e.Length = left-from, right-left
			part.entities = append(part.entities, e)
		}
	}

	return part, next
}

func (r *relay) forwardMax(ctx context.Context, src *maxSource, message maxMessage, book *deliveryBook) {
	log := slog.With("channel", src.channel.title, "post", book.post)
	needed := make([]bool, len(src.to))
	for i, dst := range src.to {
		var err error
		needed[i], err = book.needsDelivery(ctx, channelKey{telegramPlatform, dst.ID})
		if err != nil {
			return
		}
	}
	if !slices.Contains(needed, true) {
		return
	}
	body := message.Body
	text := maxPostText(message)
	lost := false
	if message.Link != nil && message.Link.Type == "forward" {
		lost = true
		// A forwarded post can have no text or attachments of its own.
		body.Attachments = append(body.Attachments, message.Link.Message.Attachments...)
	}
	if slices.ContainsFunc(body.Attachments, func(a maxMedia) bool {
		return a.Type == model.AttachImage || a.Type == model.AttachVideo || a.Type == model.AttachAudio || a.Type == model.AttachFile
	}) {
		select {
		case r.mediaSlot <- struct{}{}:
			defer func() { <-r.mediaSlot }()
		case <-ctx.Done():
			return
		}
	}
	var files []maxFile
	defer func() {
		for _, f := range files {
			_ = os.Remove(f.path)
		}
	}()
	var problems []string
	var refreshed maxMessage
	refreshTried := false
	remaining := r.maxPostLimit
	for i, a := range body.Attachments {
		switch a.Type {
		case model.AttachShare:
			continue // Telegram builds previews from the links in the text
		case model.AttachImage, model.AttachVideo, model.AttachAudio, model.AttachFile:
		default:
			lost = true

			continue
		}
		limit := min(r.maxFileLimit, remaining)
		file, err := r.downloadMax(ctx, a, limit)
		if errors.Is(err, errMaxMediaExpired) && message.Body.Mid != "" && i < len(message.Body.Attachments) {
			if !refreshTried {
				refreshTried = true
				if err := r.max.get(ctx, "/messages/"+url.PathEscape(message.Body.Mid), &refreshed); err != nil {
					log.Warn("не удалось обновить ссылки на вложения MAX", "err", err)
				}
			}
			for j, fresh := range refreshed.Body.Attachments {
				if fresh.Type == a.Type && ((a.Payload.Token != "" && fresh.Payload.Token == a.Payload.Token) || (a.Payload.Token == "" && i == j)) {
					file, err = r.downloadMax(ctx, fresh, limit)

					break
				}
			}
		}
		if err != nil {
			log.Warn("вложение MAX не скачано", "type", a.Type, "err", err)
			problems = append(problems, fmt.Sprintf("%s: %v", a.Type, err))
			lost = true

			continue
		}
		files = append(files, file)
		files[len(files)-1].sourceID = fmt.Sprintf("%s:%d", body.Mid, i)
		remaining -= file.size
	}
	if lost && webLink(message.URL) {
		text.append(linkText("Открыть пост в MAX", message.URL))
	}
	for i, dst := range src.to {
		if !needed[i] {
			continue
		}
		var plan []plannedMessage
		for _, msg := range telegramPostMessages(text, files) {
			part := plannedMessage{Text: msg.text.text, Entities: msg.text.entities, Quiet: msg.quiet}
			for _, file := range msg.files {
				part.Media = append(part.Media, plannedMedia{file.sourceID, string(file.kind)})
			}
			plan = append(plan, part)
		}
		if err := book.checkPlan(ctx, channelKey{telegramPlatform, dst.ID}, plan); err != nil {
			log.Error("продолжение публикации остановлено", "to", dst.Title, "err", err)
			return
		}
		err := book.deliver(ctx, channelKey{telegramPlatform, dst.ID}, func(publish publish) error {
			err := r.sendTelegramPost(ctx, dst.ID, text, files, publish)
			if err == nil && (len(files) > 0 || len(text.units) > 0) {
				log.Info("переслано в Telegram", "to", dst.Title)
			}

			return err
		})
		if err != nil {
			log.Error("не отправлено в Telegram", "to", dst.Title, "err", err)
			problems = append(problems, fmt.Sprintf("«%s»: %v", dst.Title, err))
		}
	}
	if len(problems) > 0 && ctx.Err() == nil {
		ref := message.URL
		if !webLink(ref) {
			ref = fmt.Sprintf("«%s», %s", src.channel.title, book.post)
		}
		_ = r.alert(ctx, fmt.Sprintf("пост %s перенесён с ошибками:\n— %s", ref, strings.Join(problems, "\n— ")))
	}
}

var (
	errMaxMediaExpired = errors.New("ссылка на вложение MAX недоступна")
	errMaxMediaSize    = errors.New("вложение MAX превышает лимит пересылки")
)

func (r *relay) downloadMax(ctx context.Context, a maxMedia, limit int64) (maxFile, error) {
	if size := cmp.Or(a.Payload.Size, int64(a.Size)); limit <= 0 || size > limit {
		return maxFile{}, errMaxMediaSize
	}
	address := a.Payload.URL
	if a.Type == model.AttachVideo && a.Payload.Token != "" {
		var details model.VideoAttachmentDetails
		// Without the details the video falls back to the URL from the message.
		if err := retry(ctx, func() (err error) {
			details, err = r.max.api.Messages.GetVideoAttachmentDetails(ctx, a.Payload.Token)

			return err
		}); err != nil {
			slog.Warn("не удалось получить ссылки на видео MAX", "err", err)
		}
		if u := details.Urls; u != nil {
			for _, mp4 := range []*string{u.Mp41080, u.Mp4720, u.Mp4480, u.Mp4360, u.Mp4240, u.Mp4144} {
				if mp4 != nil && *mp4 != "" {
					address = *mp4

					break
				}
			}
		}
	}
	var file maxFile
	err := retry(ctx, func() (err error) {
		file, err = r.fetchMaxFile(ctx, a, address, limit)

		return err
	})

	return file, err
}

func (r *relay) fetchMaxFile(ctx context.Context, a maxMedia, address string, limit int64) (maxFile, error) {
	if !strings.HasPrefix(address, "https://") || !webLink(address) {
		return maxFile{}, errMaxMediaExpired
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return maxFile{}, errMaxMediaExpired
	}
	resp, err := r.maxMediaHTTP.Do(req)
	if err != nil {
		return maxFile{}, withoutURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if definitivelyRejected(resp.StatusCode) && resp.StatusCode != http.StatusTooManyRequests {
			return maxFile{}, errMaxMediaExpired
		}

		return maxFile{}, fmt.Errorf("скачивание MAX: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return maxFile{}, errMaxMediaSize
	}
	kind := a.Type
	name := cmp.Or(a.Payload.FileName, a.FileName)
	if name == "" {
		_, disposition, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		name = cmp.Or(disposition["filename"], map[model.AttachmentType]string{
			model.AttachImage: "photo.jpg", model.AttachVideo: "video.mp4", model.AttachAudio: "audio.ogg", model.AttachFile: "file",
		}[kind])
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}

		return r
	}, name)
	file, err := os.CreateTemp(r.mediaDir, "download-")
	if err != nil {
		return maxFile{}, withoutPath(err)
	}
	size, err := io.Copy(file, io.LimitReader(resp.Body, limit+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil || size > limit {
		_ = os.Remove(file.Name())
		if size > limit {
			return maxFile{}, errMaxMediaSize
		}

		return maxFile{}, withoutPath(errors.Join(err, closeErr))
	}

	downloaded := maxFile{kind: kind, path: file.Name(), name: cmp.Or(name, "file"), size: size}
	prepared, err := downloaded.forTelegram()
	if err != nil {
		_ = os.Remove(file.Name())
	}
	return prepared, err
}

func visual(f maxFile) bool { return f.kind == model.AttachImage || f.kind == model.AttachVideo }

type telegramPostPart struct {
	text  telegramText
	files []maxFile
	quiet bool
}

func telegramPostMessages(text richText, files []maxFile) []telegramPostPart {
	var messages []telegramPostPart
	from, sent := 0, false
	for len(files) > 0 {
		n := 1
		if visual(files[0]) {
			for n < min(10, len(files)) && visual(files[n]) {
				n++
			}
		}
		caption := telegramText{}
		if !sent {
			caption, from = text.telegramPart(from, 1024)
		}
		messages = append(messages, telegramPostPart{caption, files[:n], sent})
		sent, files = true, files[n:]
	}
	for from < len(text.units) {
		part, next := text.telegramPart(from, 4096)
		if part.text != "" {
			messages = append(messages, telegramPostPart{part, nil, sent})
			sent = true
		}
		from = next
	}

	return messages
}

func (r *relay) sendTelegramPost(ctx context.Context, chatID int64, text richText, files []maxFile, publish publish) error {
	for _, msg := range telegramPostMessages(text, files) {
		if err := r.sendTelegram(ctx, chatID, msg.text, msg.files, msg.quiet, publish); err != nil {
			return err
		}
	}
	return nil
}

func (r *relay) sendTelegram(ctx context.Context, chatID int64, text telegramText, files []maxFile, quiet bool, publish publish) error {
	params := map[string]any{"chat_id": chatID, "disable_notification": quiet}
	method := "sendMessage"
	formatted := params
	entityKey := "entities"
	switch len(files) {
	case 0:
		params["text"] = text.text
	case 1:
		field := "document"
		switch files[0].kind {
		case model.AttachImage:
			field = "photo"
		case model.AttachVideo:
			field = "video"
			params["supports_streaming"] = true
		}
		method = "send" + strings.ToUpper(field[:1]) + field[1:]
		params[field] = "attach://" + fileField(0)
		params["caption"] = text.text
		entityKey = "caption_entities"
	default:
		method = "sendMediaGroup"
		album := make([]map[string]any, 0, len(files))
		for i, file := range files {
			item := map[string]any{"type": "photo", "media": "attach://" + fileField(i)}
			if file.kind == model.AttachVideo {
				item["type"] = "video"
				item["supports_streaming"] = true
			}
			album = append(album, item)
		}
		album[0]["caption"] = text.text
		params["media"] = album
		formatted = album[0]
		entityKey = "caption_entities"
	}
	if len(text.entities) > 0 {
		formatted[entityKey] = text.entities
	}
	kinds := make([]mirrorKind, max(1, len(files)))
	for i := range kinds {
		kinds[i] = mirrorMedia
	}
	if len(files) == 0 {
		kinds[0] = mirrorText
	} else if !quiet {
		kinds[0] = mirrorCaption
	}
	return publish(ctx, func() ([]string, error) {
		return retryPublish(ctx, func() ([]string, error) {
			var messages []struct {
				ID int `json:"message_id"`
			}
			var err error
			if method == "sendMediaGroup" {
				err = r.tgAPI.call(ctx, method, params, files, &messages)
			} else {
				messages = append(messages, struct {
					ID int `json:"message_id"`
				}{})
				err = r.tgAPI.call(ctx, method, params, files, &messages[0])
			}
			if err != nil {
				return nil, err
			}
			ids := make([]string, len(messages))
			missing := len(messages) != max(1, len(files))
			for i, message := range messages {
				if message.ID == 0 {
					missing = true

					continue
				}
				ids[i] = strconv.Itoa(message.ID)
			}
			if missing {
				return ids, errors.New("неполный список id в ответе Telegram")
			}

			return ids, nil
		})
	}, kinds...)
}
