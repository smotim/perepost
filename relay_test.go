package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
)

// sent is a message that reached the fake MAX.
type sent struct {
	chatID string
	body   struct {
		Text        string `json:"text"`
		Format      string `json:"format"`
		Notify      *bool  `json:"notify"`
		Attachments []struct {
			Type    string `json:"type"`
			Payload struct {
				Token string `json:"token"`
			} `json:"payload"`
		} `json:"attachments"`
	}
}

// fake is a fake Telegram and MAX with a relay and a source wired to them:
// posts of channel -100 go to MAX channel -42.
type fake struct {
	relay     *relay
	source    *source
	maxClient *maxClient

	tgAdmin, maxAdmin, maxRejects atomic.Bool
	uploads                       atomic.Int32

	mu     sync.Mutex
	sent   []sent
	alerts []string
}

func (f *fake) messages() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.sent)
}

func (f *fake) alerted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.alerts)
}

func (f *fake) forward(msgs ...*models.Message) {
	f.relay.forward(context.Background(), f.source, &post{messages: msgs})
}

func fakes(t *testing.T) *fake {
	t.Helper()
	f := &fake{}
	f.tgAdmin.Store(true)
	f.maxAdmin.Store(true)

	tgServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			// A local Bot API server answers with an absolute path on the shared volume
			path := "files/" + r.FormValue("file_id")
			if strings.HasPrefix(r.FormValue("file_id"), "/") {
				path = r.FormValue("file_id")
			}
			fmt.Fprintf(w, `{"ok":true,"result":{"file_id":%q,"file_path":%q}}`, r.FormValue("file_id"), path)
		case strings.Contains(r.URL.Path, "/file/bot"):
			fmt.Fprint(w, "bytes of "+r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		case strings.HasSuffix(r.URL.Path, "/getChatMember"):
			status := "left"
			if f.tgAdmin.Load() {
				status = "administrator"
			}
			fmt.Fprintf(w, `{"ok":true,"result":{"status":%q,"user":{"id":123,"is_bot":true,"first_name":"b"}}}`, status)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			if r.FormValue("chat_id") != "777" {
				t.Errorf("alert sent to %q", r.FormValue("chat_id"))
			}
			f.mu.Lock()
			f.alerts = append(f.alerts, r.FormValue("text"))
			f.mu.Unlock()
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":777,"type":"private"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(tgServer.Close)

	var maxServer *httptest.Server
	maxServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("type")
		switch {
		case r.URL.Path == "/uploads":
			fmt.Fprintf(w, `{"url":%q,"token":"%s-token"}`, maxServer.URL+"/upload/"+kind, kind)
		case strings.HasPrefix(r.URL.Path, "/upload/"):
			f.uploads.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			switch r.URL.Path {
			case "/upload/image":
				fmt.Fprint(w, `{"photos":{"a":{"token":"image-token"}}}`)
			case "/upload/file":
				fmt.Fprint(w, `{"token":"file-token"}`)
			}
		case strings.HasSuffix(r.URL.Path, "/members/me"):
			fmt.Fprintf(w, `{"user_id":1,"is_admin":%t}`, f.maxAdmin.Load())
		case r.URL.Path == "/messages":
			if f.maxRejects.Load() {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"code":"proto.payload","message":"text is too long"}`)

				return
			}
			m := sent{chatID: r.URL.Query().Get("chat_id")}
			if err := json.NewDecoder(r.Body).Decode(&m.body); err != nil {
				t.Errorf("message body: %v", err)
			}
			f.mu.Lock()
			f.sent = append(f.sent, m)
			f.mu.Unlock()
			fmt.Fprint(w, `{"message":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(maxServer.Close)

	tg, err := bot.New("123:secret", bot.WithSkipGetMe(), bot.WithServerURL(tgServer.URL))
	if err != nil {
		t.Fatal(err)
	}
	api, err := maxbot.NewApi("max-token", maxbot.WithBaseURL(maxServer.URL))
	if err != nil {
		t.Fatal(err)
	}
	f.maxClient = &maxClient{api: api, http: maxServer.Client(), token: "max-token"}
	f.relay = &relay{
		tg:        tg,
		http:      tgServer.Client(),
		adminID:   777,
		fileLimit: tgDownloadLimit,
	}
	f.source = &source{
		channel: &models.ChatFullInfo{ID: -100, Title: "Канал", Username: "channel"},
		to:      []destination{&maxChannel{client: f.maxClient, id: -42, title: "Канал MAX"}},
	}

	return f
}

func TestAlbumGoesAsOneMessageWithCaption(t *testing.T) {
	f := fakes(t)
	f.forward(
		&models.Message{ID: 11, MediaGroupID: "g", Video: &models.Video{FileID: "v1"}},
		&models.Message{ID: 10, MediaGroupID: "g", Photo: []models.PhotoSize{{FileID: "small"}, {FileID: "big"}},
			Caption: "Афиша недели", CaptionEntities: []models.MessageEntity{entity(models.MessageEntityTypeBold, 0, 5)}},
	)

	got := f.messages()
	if len(got) != 1 {
		t.Fatalf("want 1 message, got %d", len(got))
	}
	m := got[0]
	if m.body.Text != "<b>Афиша</b> недели" || m.body.Format != "html" || m.chatID != "-42" {
		t.Fatalf("text %q format %q chat %q", m.body.Text, m.body.Format, m.chatID)
	}
	var kinds []string
	for _, a := range m.body.Attachments {
		kinds = append(kinds, a.Type+":"+a.Payload.Token)
	}
	if strings.Join(kinds, ",") != "image:image-token,video:video-token" {
		t.Fatalf("attachments %v", kinds)
	}
}

func TestFilesGoSeparatelyAndOnlyFirstMessageNotifies(t *testing.T) {
	f := fakes(t)
	f.forward(
		&models.Message{ID: 1, MediaGroupID: "d", Document: &models.Document{FileID: "d1", FileName: "a.pdf"}, Caption: "Программа"},
		&models.Message{ID: 2, MediaGroupID: "d", Document: &models.Document{FileID: "d2", FileName: "b.pdf"}},
	)

	got := f.messages()
	if len(got) != 2 {
		t.Fatalf("want 2 messages, got %d", len(got))
	}
	if got[0].body.Text != "Программа" || len(got[0].body.Attachments) != 1 || got[0].body.Notify != nil {
		t.Fatalf("first message: %+v", got[0].body)
	}
	if got[1].body.Text != "" || len(got[1].body.Attachments) != 1 || got[1].body.Notify == nil || *got[1].body.Notify {
		t.Fatalf("second message: %+v", got[1].body)
	}
}

func TestPostGoesToEveryDestination(t *testing.T) {
	f := fakes(t)
	f.source.to = append(f.source.to, &maxChannel{client: f.maxClient, id: -43, title: "Второй канал MAX"})
	f.forward(&models.Message{ID: 5, Caption: "Фото", Photo: []models.PhotoSize{{FileID: "p"}}})

	got := f.messages()
	if len(got) != 2 || got[0].chatID != "-42" || got[1].chatID != "-43" {
		t.Fatalf("got %+v", got)
	}
	// Downloaded once, uploaded for each destination
	if n := f.uploads.Load(); n != 2 {
		t.Fatalf("uploads %d", n)
	}
}

func TestUnsupportedContentLeavesLinkToPost(t *testing.T) {
	f := fakes(t)
	f.forward(&models.Message{ID: 7, Poll: &models.Poll{Question: "Куда идём?"}})

	got := f.messages()
	want := `<a href="https://t.me/channel/7">Открыть пост в Telegram</a>`
	if len(got) != 1 || got[0].body.Text != want {
		t.Fatalf("got %+v", got)
	}
}

func TestServiceMessageIsSkipped(t *testing.T) {
	f := fakes(t)
	f.forward(&models.Message{ID: 3, NewChatTitle: "Новое название"})

	if got := f.messages(); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestRepostSaysWhereItCameFrom(t *testing.T) {
	f := fakes(t)
	f.forward(&models.Message{
		ID:   8,
		Text: "Чужая новость",
		ForwardOrigin: &models.MessageOrigin{
			Type: models.MessageOriginTypeChannel,
			MessageOriginChannel: &models.MessageOriginChannel{
				Chat:      models.Chat{Title: "Источник", Username: "source"},
				MessageID: 5,
			},
		},
	})

	got := f.messages()
	want := "<i>Переслано из <a href=\"https://t.me/source/5\">Источник</a></i>\n\nЧужая новость"
	if len(got) != 1 || got[0].body.Text != want {
		t.Fatalf("got %+v", got)
	}
}

func TestFailedPostIsReportedToAdmin(t *testing.T) {
	f := fakes(t)
	f.maxRejects.Store(true)
	f.forward(&models.Message{ID: 9, Text: "Пост"})

	alerts := f.alerted()
	if len(alerts) != 1 || !strings.Contains(alerts[0], "пост https://t.me/channel/9 перенесён с ошибками") ||
		!strings.Contains(alerts[0], "«Канал MAX»") || !strings.Contains(alerts[0], "text is too long") {
		t.Fatalf("alerts %q", alerts)
	}
}

func TestTooBigVideoIsLinkedAndReported(t *testing.T) {
	f := fakes(t)
	f.forward(&models.Message{ID: 4, Caption: "Запись лекции", Video: &models.Video{FileID: "v", FileSize: 300 << 20}})

	got := f.messages()
	want := "Запись лекции\n\n<a href=\"https://t.me/channel/4\">Открыть пост в Telegram</a>"
	if len(got) != 1 || got[0].body.Text != want {
		t.Fatalf("got %+v", got)
	}
	if alerts := f.alerted(); len(alerts) != 1 || !strings.Contains(alerts[0], "video.mp4") {
		t.Fatalf("alerts %q", alerts)
	}
}

func TestLocalBotAPIFileIsStreamedFromDiskAndRemoved(t *testing.T) {
	f := fakes(t)
	f.relay.fileLimit = 0 // own Bot API server: no 20 MB limit
	path := filepath.Join(t.TempDir(), "bot-token", "videos", "file_0.mp4")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("video bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	f.forward(&models.Message{ID: 12, Caption: "Запись", Video: &models.Video{FileID: path, FileSize: 300 << 20}})

	got := f.messages()
	if len(got) != 1 || got[0].body.Text != "Запись" || len(got[0].body.Attachments) != 1 || got[0].body.Attachments[0].Type != "video" {
		t.Fatalf("got %+v", got)
	}
	if alerts := f.alerted(); len(alerts) != 0 {
		t.Fatalf("alerts %q", alerts)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("downloaded file is still on the volume: %v", err)
	}
}

func TestMissingLocalFileErrorHidesThePath(t *testing.T) {
	f := fakes(t)
	f.relay.fileLimit = 0
	path := filepath.Join(t.TempDir(), "123:secret-token", "videos", "gone.mp4")

	f.forward(&models.Message{ID: 13, Caption: "Запись", Video: &models.Video{FileID: path}})

	alerts := f.alerted()
	if len(alerts) != 1 || strings.Contains(alerts[0], "secret-token") || !strings.Contains(alerts[0], "общем томе") {
		t.Fatalf("alerts %q", alerts)
	}
}
