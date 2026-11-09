package main

import (
	"cmp"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/go-telegram/bot/models"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// media is an attachment of a Telegram post and what it becomes in MAX.
type media struct {
	fileID                  string
	size                    int64
	name                    string
	upload                  model.UploadType
	attach                  model.AttachmentType
	width, height, duration int
}

func mediaOf(m *models.Message) (media, bool) {
	image := func(sizes []models.PhotoSize) media {
		p := sizes[len(sizes)-1] // the largest size
		return media{fileID: p.FileID, size: int64(p.FileSize), name: "photo.jpg", upload: model.UploadImage, attach: model.AttachImage, width: p.Width, height: p.Height}
	}
	video := func(fileID string, size int64, name, fallback string) media {
		return media{fileID: fileID, size: size, name: cmp.Or(name, fallback), upload: model.UploadVideo, attach: model.AttachVideo}
	}
	file := func(fileID string, size int64, name, fallback string) media {
		return media{fileID: fileID, size: size, name: cmp.Or(name, fallback), upload: model.UploadFile, attach: model.AttachFile}
	}

	switch {
	case len(m.Photo) > 0:
		return image(m.Photo), true
	case m.LivePhoto != nil && len(m.LivePhoto.Photo) > 0:
		return image(m.LivePhoto.Photo), true
	case m.Video != nil:
		return video(m.Video.FileID, m.Video.FileSize, m.Video.FileName, "video.mp4"), true
	// Before Document: Telegram fills both fields for a GIF
	case m.Animation != nil:
		a := m.Animation
		md := video(a.FileID, a.FileSize, a.FileName, "animation.mp4")
		md.width, md.height = a.Width, a.Height
		if a.MimeType == "image/gif" || (a.MimeType == "" && strings.EqualFold(filepath.Ext(a.FileName), ".gif")) {
			md.upload, md.attach = model.UploadImage, model.AttachImage
			md.name = cmp.Or(a.FileName, "animation.gif")
		}
		return md, true
	case m.VideoNote != nil:
		return video(m.VideoNote.FileID, int64(m.VideoNote.FileSize), "", "video_note.mp4"), true
	case m.Voice != nil:
		return media{fileID: m.Voice.FileID, size: m.Voice.FileSize, name: "voice.ogg", upload: model.UploadAudio, attach: model.AttachAudio, duration: m.Voice.Duration}, true
	case m.Audio != nil:
		return file(m.Audio.FileID, m.Audio.FileSize, m.Audio.FileName, "audio.mp3"), true
	case m.Document != nil:
		return file(m.Document.FileID, m.Document.FileSize, m.Document.FileName, "file"), true
	}

	return media{}, false
}

var errMaxMediaInvalid = errors.New("вложение не соответствует ограничениям MAX")

func (m media) validate(size int64) error {
	limit := int64(4_000_000_000)
	switch m.upload {
	case model.UploadImage:
		limit = 50_000_000
		if m.width > 7680 || m.height > 7680 {
			return fmt.Errorf("%w: изображение больше 7680 × 7680", errMaxMediaInvalid)
		}
	case model.UploadVideo:
		limit = 250_000_000
	case model.UploadAudio:
		limit = 256_000_000
		if m.duration > 3600 {
			return fmt.Errorf("%w: аудио длиннее 60 минут", errMaxMediaInvalid)
		}
	}
	if size > limit {
		return fmt.Errorf("%w: %s больше %d МБ", errMaxMediaInvalid, m.upload, limit/1_000_000)
	}
	return nil
}

// Telegram can return GIF animation bytes even when the name ends in .mp4.
func (m media) prepared(file tgFile) (media, error) {
	if m.upload == model.UploadVideo || m.upload == model.UploadImage {
		r, err := file.open()
		if err != nil {
			return m, err
		}
		defer func() { _ = r.Close() }()
		var head [512]byte
		n, readErr := io.ReadFull(r, head[:])
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return m, readErr
		}
		if n >= 6 && (string(head[:6]) == "GIF87a" || string(head[:6]) == "GIF89a") {
			m.upload, m.attach = model.UploadImage, model.AttachImage
			m.name = strings.TrimSuffix(m.name, filepath.Ext(m.name)) + ".gif"
		} else if http.DetectContentType(head[:n]) == "video/mp4" && m.upload == model.UploadImage {
			m.upload, m.attach = model.UploadVideo, model.AttachVideo
			m.name = strings.TrimSuffix(m.name, filepath.Ext(m.name)) + ".mp4"
		}
	}
	if m.upload == model.UploadImage {
		r, err := file.open()
		if err != nil {
			return m, err
		}
		cfg, _, decodeErr := image.DecodeConfig(r)
		_ = r.Close()
		if decodeErr == nil {
			m.width, m.height = cfg.Width, cfg.Height
		}
	}
	return m, m.validate(file.size)
}

// unsupported is content not mirrored by perepost. Some types exist on MAX
// too, but need a separate conversion. Stickers and service messages are skipped.
func unsupported(m *models.Message) bool {
	return m.Poll != nil || m.Location != nil || m.Contact != nil || m.PaidMedia != nil ||
		m.Story != nil || m.Game != nil || m.Checklist != nil || m.Giveaway != nil ||
		m.Invoice != nil || m.RichMessage != nil
}

// forwardedFrom is the "Переслано из …" line of a repost: without it someone
// else's post would look like the channel's own in MAX.
func forwardedFrom(m *models.Message) (richText, bool) {
	o := m.ForwardOrigin
	if o == nil {
		return richText{}, false
	}

	var prefix, name, link string
	switch {
	case o.MessageOriginChannel != nil:
		c := o.MessageOriginChannel
		prefix, name = "Переслано из ", c.Chat.Title
		if c.Chat.Username != "" {
			link = fmt.Sprintf("https://t.me/%s/%d", c.Chat.Username, c.MessageID)
		}
	case o.MessageOriginChat != nil:
		c := o.MessageOriginChat.SenderChat
		prefix, name = "Переслано из ", c.Title
		if c.Username != "" {
			link = "https://t.me/" + c.Username
		}
	case o.MessageOriginUser != nil:
		u := o.MessageOriginUser.SenderUser
		prefix, name = "Переслано от ", strings.TrimSpace(u.FirstName+" "+u.LastName)
		if u.Username != "" {
			link = "https://t.me/" + u.Username
		}
	case o.MessageOriginHiddenUser != nil:
		prefix, name = "Переслано от ", o.MessageOriginHiddenUser.SenderUserName
	}
	if name == "" {
		return richText{}, false
	}

	t := newRichText(prefix+name, nil)
	t.entities = append(t.entities, models.MessageEntity{Type: models.MessageEntityTypeItalic, Length: len(t.units)})
	if link != "" {
		offset := len(utf16.Encode([]rune(prefix)))
		t.entities = append(t.entities, models.MessageEntity{
			Type: models.MessageEntityTypeTextLink, Offset: offset, Length: len(t.units) - offset, URL: link,
		})
	}

	return t, true
}
