package main

import (
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"regexp"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

var validMaxUsername = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

func telegramPhotoFits(width, height int) bool {
	return width > 0 && height > 0 && width+height <= 10000 && width <= 20*height && height <= 20*width
}

// Keep unsupported media intact as a document instead of rejecting a whole
// album. Inspect the downloaded bytes: filenames and Content-Type can lie.
func (f maxFile) forTelegram() (maxFile, error) {
	if !visual(f) {
		return f, nil
	}
	file, err := os.Open(f.path)
	if err != nil {
		return f, withoutPath(err)
	}
	defer func() { _ = file.Close() }()
	usable := false
	switch f.kind {
	case model.AttachImage:
		cfg, format, err := image.DecodeConfig(file)
		usable = err == nil && (format == "jpeg" || format == "png") && f.size <= 10<<20 && telegramPhotoFits(cfg.Width, cfg.Height)
	case model.AttachVideo:
		var header [512]byte
		n, _ := io.ReadFull(file, header[:])
		usable = http.DetectContentType(header[:n]) == "video/mp4"
	}
	if !usable {
		f.kind = model.AttachFile
	}
	return f, nil
}
