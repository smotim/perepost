package main

import (
	"cmp"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/go-telegram/bot/models"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// media is an attachment of a Telegram post and what it becomes in MAX.
type media struct {
	fileID string
	size   int64
	name   string
	upload model.UploadType
	attach model.AttachmentType
}

func mediaOf(m *models.Message) (media, bool) {
	image := func(sizes []models.PhotoSize) media {
		p := sizes[len(sizes)-1] // the largest size
		return media{p.FileID, int64(p.FileSize), "photo.jpg", model.UploadImage, model.AttachImage}
	}
	video := func(fileID string, size int64, name, fallback string) media {
		return media{fileID, size, cmp.Or(name, fallback), model.UploadVideo, model.AttachVideo}
	}
	file := func(fileID string, size int64, name, fallback string) media {
		return media{fileID, size, cmp.Or(name, fallback), model.UploadFile, model.AttachFile}
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
		return video(m.Animation.FileID, m.Animation.FileSize, m.Animation.FileName, "animation.mp4"), true
	case m.VideoNote != nil:
		return video(m.VideoNote.FileID, int64(m.VideoNote.FileSize), "", "video_note.mp4"), true
	case m.Voice != nil:
		return media{m.Voice.FileID, m.Voice.FileSize, "voice.ogg", model.UploadAudio, model.AttachAudio}, true
	case m.Audio != nil:
		return file(m.Audio.FileID, m.Audio.FileSize, m.Audio.FileName, "audio.mp3"), true
	case m.Document != nil:
		return file(m.Document.FileID, m.Document.FileSize, m.Document.FileName, "file"), true
	}

	return media{}, false
}

// unsupported is content MAX has no counterpart for. Stickers and service
// messages are not listed: they are skipped silently.
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
