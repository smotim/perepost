package main

import (
	"fmt"
	stdhtml "html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

func TestMaxRequiresWritePermissionForPublication(t *testing.T) {
	for _, tc := range []struct {
		name, membership string
		allowed          bool
	}{
		{"reader", `{"is_admin":true,"permissions":["read_all_messages"]}`, false},
		{"writer", `{"is_admin":true,"permissions":["write"]}`, true},
		{"legacy writer", `{"is_admin":true,"permissions":["post_edit_delete_message"]}`, true},
		{"owner", `{"is_owner":true}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tc.membership) }))
			t.Cleanup(server.Close)
			api, err := maxbot.NewApi("token", maxbot.WithBaseURL(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			m := &maxClient{api: api}
			if err := m.checkAdmin(t.Context(), maxChat{id: -42, kind: model.ChatTypeChannel}); (err == nil) != tc.allowed {
				t.Fatalf("allowed=%t: %v", tc.allowed, err)
			}
		})
	}
}

func TestRealGIFUsesMaxImageUpload(t *testing.T) {
	md, ok := mediaOf(&models.Message{Animation: &models.Animation{FileID: "gif", FileName: "animation.gif", MimeType: "image/gif"}})
	if !ok || md.upload != model.UploadImage || md.attach != model.AttachImage {
		t.Fatalf("GIF mapped to %+v", md)
	}
}

func TestExpensiveLinkKeepsAddressAndFitsEveryPart(t *testing.T) {
	address := "https://example.org/?" + strings.Repeat("&", 1000)
	text := newRichText("🎤 Билет", []models.MessageEntity{{Type: models.MessageEntityTypeTextLink, Length: 8, URL: address}})
	parts := text.split(maxTextLimit)
	var visible strings.Builder
	for _, part := range parts {
		if htmlLen(part) > maxTextLimit || strings.ContainsRune(part, '�') {
			t.Fatalf("invalid part length=%d", htmlLen(part))
		}
		visible.WriteString(stdhtml.UnescapeString(tagRe.ReplaceAllString(part, "")))
	}
	if !strings.Contains(visible.String(), address) || !strings.HasPrefix(visible.String(), "🎤 Билет") {
		t.Fatal("label or address lost")
	}
}

func TestMaxHeadingsHighlightsAndMentionsKeepUTF16Offsets(t *testing.T) {
	text := "🙂 Заголовок имя акцент"
	body := maxMessageBody{Text: text, Markup: []maxMarkup{
		{Type: "heading", From: 3, Length: 9},
		{Type: "user_mention", From: 13, Length: 3, UserLink: "@some_user"},
		{Type: "highlighted", From: 17, Length: 6},
	}}
	got := body.richText()
	if len(got.entities) != 3 {
		t.Fatalf("entities %+v", got.entities)
	}
	if got.entities[0].Type != models.MessageEntityTypeBold || got.entities[0].Offset != 3 || got.entities[1].URL != "https://max.ru/some_user" || got.entities[2].Type != models.MessageEntityTypeUnderline {
		t.Fatalf("entities %+v", got.entities)
	}
	// An ID belongs to MAX, not Telegram. Keep the name without fabricating
	// a Telegram user mention or converting an unsafe user_link to a URL.
	body.Markup = []maxMarkup{{Type: "user_mention", From: 13, Length: 3, UserLink: "user/../bad", URL: "https://example.org/pretend"}}
	got = body.richText()
	plain, _ := got.telegramPart(0, 4096)
	if len(got.entities) != 0 || plain.text != text {
		t.Fatal("invalid mention mapping")
	}
}

func TestNestedMarkupCannotProduceOversizedPart(t *testing.T) {
	var entities []models.MessageEntity
	for range 600 {
		entities = append(entities, entity(models.MessageEntityTypeBold, 0, 2))
	}
	text := newRichText("🎤", entities)
	parts := text.split(maxTextLimit)
	if len(parts) != 1 || parts[0] != "🎤" {
		t.Fatalf("parts %q", parts)
	}
	if len(text.entities) != 600 {
		t.Fatal("split mutated the original entities")
	}
}
