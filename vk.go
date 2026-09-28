package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	vkapi "github.com/SevereCloud/vksdk/v3/api"
	"github.com/SevereCloud/vksdk/v3/object"
)

const (
	// vkEditor is the lowest admin_level that may post on a community wall.
	// The VK API counts 1 moderator, 2 editor, 3 administrator; the vksdk
	// constants start at 0 and would let a moderator through.
	vkEditor = 2
	// vkTextLimit keeps a wall post under the VK limit of 16384 characters.
	vkTextLimit = 16000
	// vkMaxAttachments is how many attachments one wall post takes.
	vkMaxAttachments = 10
)

// vkClient posts with a user access token: VK allows wall posts and uploads
// only with a token of a community editor, not with a community token.
type vkClient struct {
	api  *vkapi.VK
	http *http.Client // uploads: a video of up to 2 GB
}

func newVKClient(token string) *vkClient {
	api := vkapi.NewVK(token)
	api.Limit = vkapi.LimitUserToken
	api.Client = &http.Client{Timeout: time.Minute}

	return &vkClient{api: api, http: &http.Client{Timeout: 30 * time.Minute}}
}

// vkGroup is a VK community whose wall the posts go to.
type vkGroup struct {
	client *vkClient
	id     int
	title  string
}

func (g *vkGroup) key() string  { return "vk:" + strconv.Itoa(g.id) }
func (g *vkGroup) name() string { return "«" + g.title + "» (VK)" }

// resolve finds a community by link (https://vk.com/name), vk:name or id and
// checks that the token may post on its wall.
func (c *vkClient) resolve(ctx context.Context, ref string) (*vkGroup, error) {
	group, err := c.group(ctx, vkGroupName(ref))
	if err != nil {
		return nil, fmt.Errorf("группа VK %q не найдена: %w", ref, err)
	}
	g := &vkGroup{client: c, id: group.ID, title: group.Name}

	return g, g.checkAdmin(ctx)
}

func (c *vkClient) group(ctx context.Context, id string) (object.GroupsGroup, error) {
	resp, err := c.api.GroupsGetByID(vkapi.Params{"group_id": id, "fields": "admin_level"}.WithContext(ctx))
	if err != nil {
		return object.GroupsGroup{}, err
	}
	if len(resp.Groups) == 0 {
		return object.GroupsGroup{}, errors.New("VK не вернул группу")
	}

	return resp.Groups[0], nil
}

// checkAdmin checks that the token's owner is still at least an editor.
func (g *vkGroup) checkAdmin(ctx context.Context) error {
	group, err := g.client.group(ctx, strconv.Itoa(g.id))
	if err != nil {
		return fmt.Errorf("не удалось проверить права в группе VK «%s»: %w", g.title, err)
	}
	if !bool(group.IsAdmin) || group.AdminLevel < vkEditor {
		return fmt.Errorf("владелец VK_TOKEN не редактор группы VK «%s»: публиковать на стене могут редакторы и администраторы", g.title)
	}

	return nil
}

func (g *vkGroup) upload(ctx context.Context, md media, file tgFile) (attachment, error) {
	ctxParams := func(p vkapi.Params) vkapi.Params { return p.WithContext(ctx) }

	switch md.kind {
	case kindImage:
		server, err := g.client.api.PhotosGetWallUploadServer(ctxParams(vkapi.Params{"group_id": g.id}))
		if err != nil {
			return attachment{}, err
		}
		var uploaded object.PhotosWallUploadResponse
		if err := g.client.postFile(ctx, server.UploadURL, "photo", md.name, file, &uploaded); err != nil {
			return attachment{}, err
		}
		saved, err := g.client.api.PhotosSaveWallPhoto(ctxParams(vkapi.Params{
			"group_id": g.id, "server": uploaded.Server, "photo": uploaded.Photo, "hash": uploaded.Hash,
		}))
		if err != nil {
			return attachment{}, err
		}
		if len(saved) == 0 {
			return attachment{}, errors.New("VK не сохранил фото")
		}

		return attachment{kind: md.kind, ref: vkRef("photo", saved[0].OwnerID, saved[0].ID, saved[0].AccessKey)}, nil

	case kindVideo:
		video, err := g.client.api.VideoSave(ctxParams(vkapi.Params{"group_id": g.id, "name": md.name, "wallpost": 0}))
		if err != nil {
			return attachment{}, err
		}
		if err := g.client.postFile(ctx, video.UploadURL, "video_file", md.name, file, nil); err != nil {
			return attachment{}, err
		}

		return attachment{kind: md.kind, ref: vkRef("video", video.OwnerID, video.VideoID, video.AccessKey)}, nil

	default: // files and audio go as documents
		server, err := g.client.api.DocsGetWallUploadServer(ctxParams(vkapi.Params{"group_id": g.id}))
		if err != nil {
			return attachment{}, err
		}
		var uploaded struct {
			File string `json:"file"`
		}
		if err := g.client.postFile(ctx, server.UploadURL, "file", md.name, file, &uploaded); err != nil {
			return attachment{}, err
		}
		saved, err := g.client.api.DocsSave(ctxParams(vkapi.Params{"file": uploaded.File, "title": md.name}))
		if err != nil {
			return attachment{}, err
		}

		return attachment{kind: md.kind, ref: vkRef("doc", saved.Doc.OwnerID, saved.Doc.ID, saved.Doc.AccessKey)}, nil
	}
}

// publish posts one wall post on behalf of the community: VK has no markup on
// walls, so the text is plain, and all attachments go together. More than ten
// attachments or a text over the limit spill into further posts.
func (g *vkGroup) publish(ctx context.Context, text richText, attachments []attachment, _ bool) (int, error) {
	parts := text.splitBy(vkTextLimit, text.plain)
	var batches [][]attachment
	for batch := range slices.Chunk(attachments, vkMaxAttachments) {
		batches = append(batches, batch)
	}

	posts := max(len(parts), len(batches))
	for i := range posts {
		params := vkapi.Params{"owner_id": -g.id, "from_group": 1}
		if i < len(parts) {
			params["message"] = parts[i]
		}
		if i < len(batches) {
			refs := make([]string, 0, len(batches[i]))
			for _, a := range batches[i] {
				refs = append(refs, a.ref)
			}
			params["attachments"] = strings.Join(refs, ",")
		}
		err := retry(ctx, func() error {
			_, err := g.client.api.WallPost(params.WithContext(ctx))

			return err
		})
		if err != nil {
			return i, fmt.Errorf("пост %d из %d не опубликован: %w", i+1, posts, err)
		}
	}

	return posts, nil
}

// postFile streams a file to a VK upload server as multipart/form-data and
// decodes the answer into out. vksdk would buffer the whole file in memory,
// which a video of up to 2 GB must not do.
func (c *vkClient) postFile(ctx context.Context, url, field, name string, file tgFile, out any) error {
	r, err := file.open()
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	var envelope bytes.Buffer
	form := multipart.NewWriter(&envelope)
	if _, err := form.CreateFormFile(field, name); err != nil {
		return err
	}
	head := envelope.Len()
	if err := form.Close(); err != nil {
		return err
	}
	tail := bytes.Clone(envelope.Bytes()[head:])
	body := io.MultiReader(bytes.NewReader(envelope.Bytes()[:head]), r, bytes.NewReader(tail))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return errors.New("не удалось собрать запрос на загрузку в VK")
	}
	req.ContentLength = int64(head) + file.size + int64(len(tail))
	req.Header.Set("Content-Type", form.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("загрузка в VK: %w", withoutURL(err))
	}
	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("загрузка в VK: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("загрузка в VK: сервер ответил %s", resp.Status)
	}
	var failure struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(answer, &failure) == nil && failure.Error != nil {
		return fmt.Errorf("загрузка в VK: %v", failure.Error)
	}
	if out == nil {
		return nil
	}

	return json.Unmarshal(answer, out)
}

func vkRef(kind string, owner, id int, accessKey string) string {
	ref := fmt.Sprintf("%s%d_%d", kind, owner, id)
	if accessKey != "" {
		ref += "_" + accessKey
	}

	return ref
}

// vkGroupName extracts what groups.getById takes from a link (https://vk.com/name,
// vk.ru/club123), vk:name or a bare name or id.
func vkGroupName(ref string) string {
	name := strings.TrimPrefix(strings.TrimSpace(ref), "vk:")
	for _, host := range []string{"vk.com", "vk.ru", "m.vk.com", "m.vk.ru"} {
		name = channelName(name, host)
	}

	return name
}

// isVK tells a VK route target from a MAX one.
func isVK(ref string) bool {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "vk:") {
		return true
	}
	for _, host := range []string{"vk.com/", "vk.ru/"} {
		if strings.Contains(ref, host) {
			return true
		}
	}

	return false
}
