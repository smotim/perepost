package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	vkapi "github.com/SevereCloud/vksdk/v3/api"
	"github.com/go-telegram/bot/models"
)

// fakeVK is a fake VK API with upload servers for community 77.
type fakeVK struct {
	adminLevel atomic.Int32

	mu      sync.Mutex
	posts   []url.Values
	uploads []string // form fields that carried a file
}

func (v *fakeVK) wallPosts() []url.Values {
	v.mu.Lock()
	defer v.mu.Unlock()

	return slices.Clone(v.posts)
}

func newFakeVK(t *testing.T) (*vkClient, *fakeVK) {
	t.Helper()
	v := &fakeVK{}
	v.adminLevel.Store(vkEditor)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if field, ok := strings.CutPrefix(r.URL.Path, "/upload/"); ok {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("upload %s: %v", field, err)
			}
			for name := range r.MultipartForm.File {
				v.mu.Lock()
				v.uploads = append(v.uploads, name)
				v.mu.Unlock()
			}
			switch field {
			case "photo":
				fmt.Fprint(w, `{"server":1,"photo":"[{}]","hash":"h"}`)
			case "video":
				fmt.Fprint(w, `{"size":11,"video_id":20}`)
			case "doc":
				fmt.Fprint(w, `{"file":"uploaded-doc"}`)
			}

			return
		}

		_ = r.ParseForm()
		switch strings.TrimPrefix(r.URL.Path, "/method/") {
		case "groups.getById":
			fmt.Fprintf(w, `{"response":{"groups":[{"id":77,"name":"Группа VK","screen_name":"my_group","is_admin":1,"admin_level":%d}],"profiles":[]}}`,
				v.adminLevel.Load())
		case "photos.getWallUploadServer":
			fmt.Fprintf(w, `{"response":{"upload_url":%q}}`, server.URL+"/upload/photo")
		case "photos.saveWallPhoto":
			fmt.Fprint(w, `{"response":[{"id":10,"owner_id":-77}]}`)
		case "video.save":
			fmt.Fprintf(w, `{"response":{"upload_url":%q,"owner_id":-77,"video_id":20}}`, server.URL+"/upload/video")
		case "docs.getWallUploadServer":
			fmt.Fprintf(w, `{"response":{"upload_url":%q}}`, server.URL+"/upload/doc")
		case "docs.save":
			if r.Form.Get("file") != "uploaded-doc" {
				t.Errorf("docs.save file %q", r.Form.Get("file"))
			}
			fmt.Fprint(w, `{"response":{"type":"doc","doc":{"id":30,"owner_id":-77}}}`)
		case "wall.post":
			v.mu.Lock()
			v.posts = append(v.posts, r.Form)
			v.mu.Unlock()
			fmt.Fprint(w, `{"response":{"post_id":1}}`)
		default:
			fmt.Fprintf(w, `{"error":{"error_code":3,"error_msg":"Unknown method %s"}}`, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client := newVKClient("vk-token")
	client.api.MethodURL = server.URL + "/method/"
	client.api.Client = server.Client()
	client.http = server.Client()

	return client, v
}

func vkDestination(t *testing.T, f *fake) *fakeVK {
	t.Helper()
	client, v := newFakeVK(t)
	group, err := client.resolve(context.Background(), "https://vk.com/my_group")
	if err != nil {
		t.Fatal(err)
	}
	f.source.to = []destination{group}

	return v
}

func TestVKGetsOnePlainPostWithTheWholeAlbum(t *testing.T) {
	f := fakes(t)
	v := vkDestination(t, f)
	link := entity(models.MessageEntityTypeTextLink, 0, 5)
	link.URL = "https://example.com/afisha"
	f.forward(
		&models.Message{ID: 11, MediaGroupID: "g", Video: &models.Video{FileID: "v1"}},
		&models.Message{ID: 10, MediaGroupID: "g", Photo: []models.PhotoSize{{FileID: "p1"}},
			Caption: "Афиша недели", CaptionEntities: []models.MessageEntity{link, entity(models.MessageEntityTypeBold, 6, 6)}},
	)

	posts := v.wallPosts()
	if len(posts) != 1 {
		t.Fatalf("want 1 wall post, got %d", len(posts))
	}
	p := posts[0]
	if p.Get("owner_id") != "-77" || p.Get("from_group") != "1" {
		t.Fatalf("post goes to %q from_group=%q", p.Get("owner_id"), p.Get("from_group"))
	}
	if want := "Афиша (https://example.com/afisha) недели"; p.Get("message") != want {
		t.Fatalf("message %q, want %q", p.Get("message"), want)
	}
	if want := "photo-77_10,video-77_20"; p.Get("attachments") != want {
		t.Fatalf("attachments %q, want %q", p.Get("attachments"), want)
	}
	if want := []string{"photo", "video_file"}; !slices.Equal(v.uploads, want) {
		t.Fatalf("uploads %v, want %v", v.uploads, want)
	}
	if alerts := f.alerted(); len(alerts) != 0 {
		t.Fatalf("alerts %q", alerts)
	}
}

func TestVKGetsFilesAsDocuments(t *testing.T) {
	f := fakes(t)
	v := vkDestination(t, f)
	f.forward(&models.Message{ID: 1, Caption: "Программа", Document: &models.Document{FileID: "d1", FileName: "program.pdf"}})

	posts := v.wallPosts()
	if len(posts) != 1 || posts[0].Get("attachments") != "doc-77_30" || posts[0].Get("message") != "Программа" {
		t.Fatalf("posts %v", posts)
	}
}

func TestVKNeedsAnEditor(t *testing.T) {
	client, v := newFakeVK(t)
	v.adminLevel.Store(1) // a moderator may not post on the wall

	_, err := client.resolve(context.Background(), "vk:my_group")
	if err == nil || !strings.Contains(err.Error(), "не редактор группы VK «Группа VK»") {
		t.Fatalf("err %v", err)
	}
}

func TestPostGoesToMaxAndVKTogether(t *testing.T) {
	f := fakes(t)
	maxChannel := f.source.to[0]
	v := vkDestination(t, f)
	f.source.to = append([]destination{maxChannel}, f.source.to...)

	f.forward(&models.Message{ID: 2, Text: "Всем привет", Entities: []models.MessageEntity{entity(models.MessageEntityTypeBold, 0, 4)}})

	if got := f.messages(); len(got) != 1 || got[0].body.Text != "<b>Всем</b> привет" {
		t.Fatalf("MAX got %+v", got)
	}
	if posts := v.wallPosts(); len(posts) != 1 || posts[0].Get("message") != "Всем привет" {
		t.Fatalf("VK got %v", posts)
	}
}

func TestVKReferences(t *testing.T) {
	for ref, want := range map[string]string{
		"https://vk.com/my_group":  "my_group",
		"vk.ru/club123":            "club123",
		"https://m.vk.com/public5": "public5",
		"vk:my_group":              "my_group",
	} {
		if !isVK(ref) {
			t.Errorf("%q is not taken for VK", ref)
		}
		if got := vkGroupName(ref); got != want {
			t.Errorf("vkGroupName(%q) = %q, want %q", ref, got, want)
		}
	}
	for _, ref := range []string{"https://max.ru/news", "-72123456789", "news"} {
		if isVK(ref) {
			t.Errorf("%q is taken for VK", ref)
		}
	}
}

func TestVKErrorsThatWillNotPassAreNotRetried(t *testing.T) {
	if !permanent(&vkapi.Error{Code: vkapi.ErrPermission}) {
		t.Error("permission denied is retried")
	}
	if permanent(&vkapi.Error{Code: vkapi.ErrTooMany}) || permanent(&vkapi.Error{Code: vkapi.ErrServer}) {
		t.Error("a transient VK error is not retried")
	}
}
