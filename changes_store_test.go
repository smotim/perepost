package main

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestKeepAlbumPreservesOriginalPartsAndNumericOrder(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	source := channelKey{telegramPlatform, -100}
	first := []byte(`{"message_id":10,"caption":"first version"}`)
	second := []byte(`{"message_id":2,"caption":"second"}`)
	if err := db.keepAlbum(ctx, source, "album:a", [][]byte{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := db.keepAlbum(ctx, source, "album:a", [][]byte{[]byte(`{"message_id":10,"caption":"replacement"}`)}); err != nil {
		t.Fatal(err)
	}
	payloads, err := db.albumPayloads(ctx, source, "album:a")
	if err != nil || len(payloads) != 2 || string(payloads[0]) != string(second) || string(payloads[1]) != string(first) {
		t.Fatalf("album snapshot changed or reordered: %q, %v", payloads, err)
	}
}

func TestReviseAlbumKeepsOtherPartsAndSeparatesLateFragments(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	source := channelKey{telegramPlatform, -100}
	other := channelKey{telegramPlatform, -200}
	for _, seed := range []struct {
		source channelKey
		post   string
		parts  []string
	}{
		{source, "album:a", []string{`{"message_id":10,"caption":"ten"}`, `{"message_id":2,"caption":"two"}`}},
		{source, "album:a#3", []string{`{"message_id":3,"caption":"late"}`}},
		{other, "album:a", []string{`{"message_id":2,"caption":"other channel"}`}},
	} {
		var payloads [][]byte
		for _, part := range seed.parts {
			payloads = append(payloads, []byte(part))
		}
		if err := db.keepAlbum(ctx, seed.source, seed.post, payloads); err != nil {
			t.Fatal(err)
		}
	}
	for _, update := range []struct {
		mid, root, message string
		want               []string
	}{
		{"2", "album:a", `{"message_id":2,"caption":"edited two"}`, []string{"edited two", "ten"}},
		{"3", "album:a#3", `{"message_id":3,"caption":"edited late"}`, []string{"edited late"}},
		{"99", "", `{"message_id":99,"caption":"unknown"}`, nil},
	} {
		root, payloads, err := db.reviseAlbum(ctx, source, change{Group: "album:a", Mid: update.mid, Message: json.RawMessage(update.message)})
		if err != nil || root != update.root {
			t.Fatalf("revision root=%q, want %q, err=%v", root, update.root, err)
		}
		var captions []string
		for _, payload := range payloads {
			var part struct{ Caption string }
			if err := json.Unmarshal(payload, &part); err != nil {
				t.Fatal(err)
			}
			captions = append(captions, part.Caption)
		}
		if !slices.Equal(captions, update.want) {
			t.Fatalf("revision %s: captions=%q, want %q", update.mid, captions, update.want)
		}
	}
	payloads, err := db.albumPayloads(ctx, other, "album:a")
	if err != nil || len(payloads) != 1 || string(payloads[0]) != `{"message_id":2,"caption":"other channel"}` {
		t.Fatalf("another channel changed: %q, %v", payloads, err)
	}
}
