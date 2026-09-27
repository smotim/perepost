package main

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-telegram/bot/models"
)

func entity(t models.MessageEntityType, offset, length int) models.MessageEntity {
	return models.MessageEntity{Type: t, Offset: offset, Length: length}
}

func html(text string, entities ...models.MessageEntity) string {
	t := newRichText(text, entities)

	return t.render(0, len(t.units))
}

func TestRenderEscapesText(t *testing.T) {
	got := html("a < b & c > d")
	if want := "a &lt; b &amp; c &gt; d"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderNestedEntitiesAndLinks(t *testing.T) {
	link := entity(models.MessageEntityTypeTextLink, 0, 6)
	link.URL = `https://example.com/?a=1&b="2"`
	got := html("Билеты тут",
		link,
		entity(models.MessageEntityTypeBold, 0, 10),
		entity(models.MessageEntityTypeItalic, 7, 3),
	)
	want := `<b><a href="https://example.com/?a=1&amp;b=&quot;2&quot;">Билеты</a> <i>тут</i></b>`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderCountsOffsetsInUTF16(t *testing.T) {
	// An emoji outside the BMP takes two UTF-16 code units
	got := html("🎉 Привет 👋", entity(models.MessageEntityTypeBold, 3, 6))
	if want := "🎉 <b>Привет</b> 👋"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderMapsTelegramOnlyEntities(t *testing.T) {
	got := html("цитата спойлер код",
		entity(models.MessageEntityTypeExpandableBlockquote, 0, 6),
		entity(models.MessageEntityTypeSpoiler, 7, 7),
		entity(models.MessageEntityTypePre, 15, 3),
	)
	if want := "<blockquote>цитата</blockquote> спойлер <code>код</code>"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderWrapsBareURLs(t *testing.T) {
	got := html("сайт example.com/a?b&c", entity(models.MessageEntityTypeURL, 5, 17))
	if want := `сайт <a href="https://example.com/a?b&amp;c">example.com/a?b&amp;c</a>`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderSameRangeEntities(t *testing.T) {
	got := html("x", entity(models.MessageEntityTypeBold, 0, 1), entity(models.MessageEntityTypeItalic, 0, 1))
	if want := "<b><i>x</i></b>"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAppendShiftsEntities(t *testing.T) {
	var text richText
	text.append(newRichText("первый", nil))
	text.append(newRichText("второй", []models.MessageEntity{entity(models.MessageEntityTypeBold, 0, 6)}))
	if got, want := text.render(0, len(text.units)), "первый\n\n<b>второй</b>"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestShortTextIsOnePart(t *testing.T) {
	parts := newRichText("  привет\n", nil).split(maxTextLimit)
	if len(parts) != 1 || parts[0] != "привет" {
		t.Fatalf("got %q", parts)
	}
}

var tagRe = regexp.MustCompile(`<[^>]+>`)

func TestSplitByParagraphsKeepsMarkupClosed(t *testing.T) {
	paragraph := strings.Repeat("слово ", 100) // 600 characters
	source := strings.TrimSpace(strings.Repeat(paragraph+"\n\n", 10))
	// Bold over the whole text: every part must close and reopen it
	text := newRichText(source, []models.MessageEntity{entity(models.MessageEntityTypeBold, 0, len(source))})

	parts := text.split(1000)
	if len(parts) < 6 {
		t.Fatalf("expected several parts, got %d", len(parts))
	}
	var words int
	for _, part := range parts {
		if htmlLen(part) > 1000 {
			t.Fatalf("part is longer than limit: %d", htmlLen(part))
		}
		if !strings.HasPrefix(part, "<b>") || !strings.HasSuffix(part, "</b>") {
			t.Fatalf("markup is not closed: %q…%q", part[:10], part[len(part)-10:])
		}
		words += len(strings.Fields(tagRe.ReplaceAllString(part, "")))
	}
	if words != 1000 {
		t.Fatalf("lost words: got %d of 1000", words)
	}
}

func TestSplitCountsMarkup(t *testing.T) {
	// 3000 visible characters, but every word carries a long link — far over
	// 4000 as HTML, like the event digest MAX once rejected
	var (
		source   strings.Builder
		entities []models.MessageEntity
	)
	for i := 0; i < 300; i++ {
		link := entity(models.MessageEntityTypeTextLink, i*10, 9) // "ссылка123 " is 10 UTF-16 code units
		link.URL = "https://example.com/" + strings.Repeat("x", 30)
		entities = append(entities, link)
		source.WriteString("ссылка123 ")
	}
	parts := newRichText(source.String(), entities).split(maxTextLimit)
	if len(parts) < 2 {
		t.Fatalf("expected split by markup length, got %d part(s)", len(parts))
	}
	for _, part := range parts {
		if htmlLen(part) > maxTextLimit {
			t.Fatalf("part is longer than limit: %d", htmlLen(part))
		}
	}
}

func TestHardCutKeepsEmojiWhole(t *testing.T) {
	parts := newRichText(strings.Repeat("🎤", 3000), nil).split(maxTextLimit)
	var emoji int
	for _, part := range parts {
		if !utf8.ValidString(part) || strings.ContainsRune(part, utf8.RuneError) {
			t.Fatalf("broken emoji in part")
		}
		emoji += utf8.RuneCountInString(part)
	}
	if emoji != 3000 {
		t.Fatalf("lost emoji: got %d of 3000", emoji)
	}
}
