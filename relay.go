package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// source is a Telegram channel together with the destinations its posts go to.
// Each source has its own queue: order matters within a channel only, and a
// long video upload in one channel must not hold back the others.
type source struct {
	channel *models.ChatFullInfo
	to      []destination
	posts   *queue
}

// postRef points a human to a post: a link for a public channel, a number otherwise.
func (s *source) postRef(id int) string {
	if s.channel.Username == "" {
		return fmt.Sprintf("«%s» №%d", s.channel.Title, id)
	}

	return "https://t.me/" + s.channel.Username + "/" + strconv.Itoa(id)
}

// relay moves posts of Telegram channels to MAX channels and VK communities.
type relay struct {
	tg        *bot.Bot
	http      *http.Client
	adminID   int64 // Telegram user to report failures to; 0 — nobody
	fileLimit int64 // largest file Telegram hands out; 0 — no limit (local Bot API server)
}

// uploads are the attachments of one post uploaded for one destination.
type uploads struct {
	attachments []attachment
	failed      bool
}

func (r *relay) forward(ctx context.Context, src *source, p *post) {
	msgs := slices.SortedFunc(slices.Values(p.messages), func(a, b *models.Message) int { return a.ID - b.ID })
	ref := src.postRef(msgs[0].ID)
	log := slog.With("channel", src.channel.Title, "post", msgs[0].ID)
	defer func() {
		if v := recover(); v != nil {
			log.Error("сбой при пересылке", "panic", v)
			_ = r.alert(fmt.Sprintf("сбой при пересылке поста %s: %v", ref, v))
		}
	}()

	var (
		text     richText
		files    []media
		lost     bool // part of the post cannot reach MAX: readers get a link to the original
		problems []string
		preview  = true
	)
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
		if md, ok := mediaOf(m); ok {
			files = append(files, md)
		} else if unsupported(m) {
			log.Warn("в MAX такого нет, часть поста пропущена")
			lost = true
		}
	}
	if len(text.units) == 0 && len(files) == 0 && !lost {
		log.Debug("пересылать нечего") // service messages: a pin, a new title

		return
	}

	// Each file is downloaded once and uploaded for every destination, so only
	// one file is held at a time.
	uploaded := make([]uploads, len(src.to))
	for _, md := range files {
		file, err := r.download(ctx, md)
		if err != nil {
			log.Error("вложение не скачано", "file", md.name, "err", err)
			problems = append(problems, fmt.Sprintf("%s: %v", md.name, err))
			lost = true

			continue
		}
		for i, dst := range src.to {
			a, err := r.upload(ctx, dst, md, file)
			if err != nil {
				log.Error("вложение не загружено", "file", md.name, "to", dst.name(), "err", err)
				problems = append(problems, fmt.Sprintf("%s → %s: %v", md.name, dst.name(), err))
				uploaded[i].failed = true

				continue
			}
			uploaded[i].attachments = append(uploaded[i].attachments, a)
		}
		file.remove()
	}

	for i, dst := range src.to {
		body := text.clone()
		// A private channel's link would not open for readers elsewhere
		if (lost || uploaded[i].failed) && src.channel.Username != "" {
			body.append(linkText("Открыть пост в Telegram", ref))
		}
		n, err := dst.publish(ctx, body, uploaded[i].attachments, preview)
		if err != nil {
			log.Error("не опубликовано", "to", dst.name(), "err", err)
			problems = append(problems, fmt.Sprintf("%s: %v", dst.name(), err))

			continue
		}
		if n > 0 {
			log.Info("переслано", "to", dst.name(), "messages", n)
		}
	}

	if len(problems) > 0 {
		_ = r.alert(fmt.Sprintf("пост %s перенесён с ошибками:\n— %s", ref, strings.Join(problems, "\n— ")))
	}
}

func (r *relay) download(ctx context.Context, md media) (tgFile, error) {
	if r.fileLimit > 0 && md.size > r.fileLimit {
		return tgFile{}, errTooBig
	}

	var file tgFile
	err := retry(ctx, func() (err error) {
		file, err = downloadFile(ctx, r.tg, r.http, md.fileID)

		return err
	})

	return file, err
}

func (r *relay) upload(ctx context.Context, dst destination, md media, file tgFile) (attachment, error) {
	var a attachment
	err := retry(ctx, func() (err error) {
		a, err = dst.upload(ctx, md, file)

		return err
	})

	return a, err
}

// alert writes to the admin in Telegram when TELEGRAM_ADMIN_ID is set.
func (r *relay) alert(text string) error {
	if r.adminID == 0 {
		return nil
	}
	if runes := []rune(text); len(runes) > 3000 {
		text = string(runes[:3000]) + "…"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := r.tg.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:             r.adminID,
		Text:               "perepost: " + text,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
	})
	if err != nil {
		slog.Warn("не удалось написать администратору", "err", err)
	}

	return err
}
