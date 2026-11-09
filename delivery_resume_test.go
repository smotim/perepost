package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func TestTelegramAlbumResumeSendsOnlyItsRemainingText(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var photo bytes.Buffer
	if err := png.Encode(&photo, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	var methods []string
	source := &maxSource{channel: maxChat{id: -42}, to: []*models.ChatFullInfo{{ID: -100}}}
	message := maxMessage{Body: maxMessageBody{Mid: "original", Text: strings.Repeat("я", 5000)}}
	for _, name := range []string{"first", "second"} {
		var media maxMedia
		media.Type = model.AttachImage
		media.Payload.URL = "https://media.example/" + name
		message.Body.Attachments = append(message.Body.Attachments, media)
	}
	r := &relay{
		adminID:  777, // an alert on cancellation would call the deliberately absent client
		mediaDir: t.TempDir(), mediaSlot: make(chan struct{}, 1), maxFileLimit: 1 << 20, maxPostLimit: 2 << 20,
		maxMediaHTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(photo.Bytes())), ContentLength: int64(photo.Len()), Header: make(http.Header), Request: req}, nil
		})},
		tgAPI: telegramAPI{baseURL: "https://telegram.example", token: "token", http: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Body != nil {
					_, _ = io.Copy(io.Discard, req.Body)
					_ = req.Body.Close()
				}
				method := strings.TrimPrefix(req.URL.Path, "/bottoken/")
				methods = append(methods, method)
				result := `{"ok":true,"result":{"message_id":13}}`
				if method == "sendMediaGroup" {
					result = `{"ok":true,"result":[{"message_id":11},{"message_id":12}]}`
				}
				body := io.NopCloser(strings.NewReader(result))
				if len(methods) == 1 {
					body = cancelOnClose{body, cancel}
				}
				return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: body, Header: make(http.Header), Request: req}, nil
			}),
		}},
	}
	path := filepath.Join(t.TempDir(), "queue.sqlite")
	db, err := openStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.close() })
	book := &deliveryBook{store: db, source: channelKey{maxPlatform, -42}, post: "message:original"}
	r.forwardMax(ctx, source, message, book)
	if book.err != nil || !slices.Equal(methods, []string{"sendMediaGroup"}) {
		t.Fatalf("interrupted after album: methods=%v, err=%v", methods, book.err)
	}
	db.close()
	db, err = openStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	book = &deliveryBook{store: db, source: channelKey{maxPlatform, -42}, post: "message:original"}
	r.forwardMax(t.Context(), source, message, book)
	if book.err != nil || !slices.Equal(methods, []string{"sendMediaGroup", "sendMessage"}) {
		t.Fatalf("resume duplicated or lost a part: methods=%v, err=%v", methods, book.err)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM mirrors`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("mirrors=%d, err=%v", count, err)
	}
}

func TestMaxTextResumeDoesNotRepublishTheFirstPart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	db := testStore(t)
	var texts []string
	r := &relay{max: &maxClient{baseURL: "https://max.example", publish: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			data, err := io.ReadAll(req.Body)
			_ = req.Body.Close()
			if err != nil {
				return nil, err
			}
			texts = append(texts, string(data))
			body := io.NopCloser(strings.NewReader(fmt.Sprintf(`{"message":{"body":{"mid":"part.%d"}}}`, len(texts))))
			if len(texts) == 1 {
				body = cancelOnClose{body, cancel}
			}
			return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header), Request: req}, nil
		}),
	}}, adminID: 777}
	source := &source{channel: &models.ChatFullInfo{ID: -100}, to: []maxChat{{id: -42}}}
	p := &post{messages: []*models.Message{{ID: 8, Text: strings.Repeat("а", maxTextLimit) + strings.Repeat("б", 100)}}}
	book := &deliveryBook{store: db, source: channelKey{telegramPlatform, -100}, post: "message:8"}
	r.forward(ctx, source, p, book)
	if book.err != nil || len(texts) != 1 {
		t.Fatalf("first pass: parts=%d, err=%v", len(texts), book.err)
	}
	r.forward(t.Context(), source, p, book)
	if book.err != nil || len(texts) != 2 || texts[0] == texts[1] || !strings.Contains(texts[1], strings.Repeat("б", 100)) {
		t.Fatalf("resume: parts=%d, err=%v", len(texts), book.err)
	}
}
