package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

func pngFixture(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func mediaFixture(t *testing.T, kind model.AttachmentType, data []byte) maxFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return maxFile{kind: kind, path: path, name: "original", size: int64(len(data))}
}

func TestUnsupportedTelegramMediaKeepsOriginalAsDocument(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind model.AttachmentType
		data []byte
		want model.AttachmentType
	}{
		{"PNG", model.AttachImage, pngFixture(t, 1, 1), model.AttachImage},
		{"wide photo", model.AttachImage, pngFixture(t, 120, 1), model.AttachFile},
		{"unknown image", model.AttachImage, []byte("TIFF or HEIC"), model.AttachFile},
		{"GIF", model.AttachImage, []byte("GIF89a"), model.AttachFile},
		{"MP4", model.AttachVideo, []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), model.AttachVideo},
		{"fallback WebM", model.AttachVideo, []byte("\x1a\x45\xdf\xa3webm"), model.AttachFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := mediaFixture(t, tc.kind, tc.data)
			got, err := file.forTelegram()
			if err != nil {
				t.Fatal(err)
			}
			if got.kind != tc.want || got.path != file.path || got.size != file.size {
				t.Fatalf("got %+v", got)
			}
			data, err := os.ReadFile(got.path)
			if err != nil || !bytes.Equal(data, tc.data) {
				t.Fatal("original bytes changed")
			}
		})
	}
}

func TestTelegramPhotoGeometryAndSizeLimits(t *testing.T) {
	for _, tc := range []struct {
		w, h int
		fits bool
	}{{5000, 5000, true}, {6000, 6000, false}, {20, 1, true}, {21, 1, false}, {1, 20, true}, {1, 21, false}, {0, 10, false}} {
		if got := telegramPhotoFits(tc.w, tc.h); got != tc.fits {
			t.Fatalf("%dx%d fits=%t", tc.w, tc.h, got)
		}
	}
	file := mediaFixture(t, model.AttachImage, pngFixture(t, 1, 1))
	file.size = 11 << 20
	got, err := file.forTelegram()
	if err != nil || got.kind != model.AttachFile {
		t.Fatalf("large image: %+v %v", got, err)
	}
}

func TestWidePhotoStillDeliversCaptionAndFollowingText(t *testing.T) {
	file, err := mediaFixture(t, model.AttachImage, pngFixture(t, 120, 1)).forTelegram()
	if err != nil {
		t.Fatal(err)
	}
	var methods, texts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		methods = append(methods, strings.TrimPrefix(req.URL.Path, "/bottoken/"))
		if strings.Contains(req.Header.Get("Content-Type"), "multipart/") {
			if err := req.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			defer func() { _ = req.MultipartForm.RemoveAll() }()
			texts = append(texts, req.FormValue("caption"))
		} else {
			var body struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			texts = append(texts, body.Text)
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, len(methods))
	}))
	t.Cleanup(server.Close)
	r := &relay{tgAPI: telegramAPI{baseURL: server.URL, token: "token", http: server.Client()}}
	var book *deliveryBook
	text := strings.Repeat("я", 1500)
	if err := book.deliver(t.Context(), channelKey{telegramPlatform, -100}, func(publish publish) error {
		return r.sendTelegramPost(t.Context(), -100, newRichText(text, nil), []maxFile{file}, publish)
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "sendDocument,sendMessage" || strings.Join(texts, "") != text {
		t.Fatalf("methods %v, text lengths %v", methods, texts)
	}
}
