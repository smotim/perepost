package main

import "context"

// mediaKind is what an attachment is, whatever the platform calls it.
type mediaKind int

const (
	kindImage mediaKind = iota
	kindVideo
	kindAudio
	kindFile
)

// attachment is a file uploaded to a platform for one destination.
type attachment struct {
	kind mediaKind
	ref  string // MAX token or VK "photo-1_2"
}

// destination is where posts go: a MAX channel or a VK community. Each
// platform decides how a post looks there — MAX gets HTML split into several
// messages, VK a single wall post in plain text.
type destination interface {
	// key tells destinations apart across platforms: "max:-72…", "vk:123".
	key() string
	// name is for people: «Title» (MAX).
	name() string
	// checkAdmin reports an error when the bot can no longer post there.
	checkAdmin(ctx context.Context) error
	// upload puts one file on the platform for this destination.
	upload(ctx context.Context, md media, file tgFile) (attachment, error)
	// publish posts the text and the uploaded attachments and returns how many
	// messages or posts went out.
	publish(ctx context.Context, text richText, attachments []attachment, preview bool) (int, error)
}
