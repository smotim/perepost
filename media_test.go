package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

func TestMaxMediaLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		m     media
		size  int64
		valid bool
	}{
		{"video at limit", media{upload: model.UploadVideo}, 250_000_000, true},
		{"large video", media{upload: model.UploadVideo}, 250_000_001, false},
		{"large audio", media{upload: model.UploadAudio}, 256_000_001, false},
		{"long voice", media{upload: model.UploadAudio, duration: 3601}, 100, false},
		{"voice at duration limit", media{upload: model.UploadAudio, duration: 3600}, 100, true},
		{"large image", media{upload: model.UploadImage}, 50_000_001, false},
		{"wide image", media{upload: model.UploadImage, width: 7681}, 100, false},
		{"tall image", media{upload: model.UploadImage, height: 7681}, 100, false},
		{"large document", media{upload: model.UploadFile}, 4_000_000_001, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.validate(tc.size)
			if (err == nil) != tc.valid || err != nil && !errors.Is(err, errMaxMediaInvalid) {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}

func TestInvalidVoiceIsRejectedBeforeDownloading(t *testing.T) {
	f := fakes(t)
	f.forward(&models.Message{ID: 1, Caption: "Запись", Voice: &models.Voice{FileID: "must-not-download", Duration: 3601}})
	got := f.messages()
	if f.uploads.Load() != 0 || len(got) != 1 || !strings.Contains(got[0].body.Text, "Открыть пост в Telegram") {
		t.Fatalf("messages %+v uploads %d", got, f.uploads.Load())
	}
	if alerts := f.alerted(); len(alerts) != 1 || !strings.Contains(alerts[0], "60 минут") {
		t.Fatalf("alerts %v", alerts)
	}
}

func TestActualSizeRejectsUnknownLargeVideoAndRemovesDownload(t *testing.T) {
	f := fakes(t)
	f.relay.fileLimit = 0
	path := filepath.Join(t.TempDir(), "large.mp4")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(250_000_001); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	f.forward(&models.Message{ID: 1, Caption: "Запись", Video: &models.Video{FileID: path}})
	if f.uploads.Load() != 0 {
		t.Fatal("oversized file uploaded")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("download remains: %v", err)
	}
	if got := f.messages(); len(got) != 1 || !strings.Contains(got[0].body.Text, "Открыть пост в Telegram") {
		t.Fatalf("got %+v", got)
	}
}

func TestGIFHeaderOverridesMisleadingExtension(t *testing.T) {
	m := media{upload: model.UploadVideo, attach: model.AttachVideo, name: "renamed.mp4"}
	got, err := m.prepared(tgFile{data: []byte("GIF89a"), size: 6})
	if err != nil || got.upload != model.UploadImage || got.name != "renamed.gif" {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestConvertedGIFRemainsMP4DespiteOriginalFilename(t *testing.T) {
	md, ok := mediaOf(&models.Message{Animation: &models.Animation{FileID: "converted", FileName: "original.gif", MimeType: "video/mp4"}})
	if !ok || md.upload != model.UploadVideo {
		t.Fatalf("converted GIF mapped as %+v", md)
	}
}
