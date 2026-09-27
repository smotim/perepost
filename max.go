package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// attachment is a file already uploaded to MAX.
type attachment struct {
	kind  model.AttachmentType
	token string
}

// maxChat is a MAX channel the bot posts to.
type maxChat struct {
	id    int64
	title string
}

type maxClient struct {
	api   *maxbot.Api
	http  *http.Client
	token string
}

// russianRootCA is the Russian Trusted Root CA of the Ministry of Digital
// Development (SHA-256 D2:6D:2D:02:…:CA:8E:CF:31, valid until 2032). Since
// July 2026 the MAX API at platform-api2.max.ru presents a certificate issued
// under it, and no standard trust store includes it.
//
//go:embed certs/russian_trusted_root_ca.pem
var russianRootCA []byte

// maxTransport trusts the system roots plus the Russian root. Only MAX
// requests use it: Telegram and everything else keep the system roots alone.
func maxTransport() (*http.Transport, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(russianRootCA) {
		return nil, errors.New("не удалось прочитать встроенный корневой сертификат Минцифры")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}

	return transport, nil
}

func newMaxClient(token string) (*maxClient, error) {
	transport, err := maxTransport()
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		// The library default of 30 s is not always enough to upload a video
		Timeout:   10 * time.Minute,
		Transport: retryableStatus{next: transport},
	}
	api, err := maxbot.NewApi(token, maxbot.WithHTTPClient(client))
	if err != nil {
		return nil, err
	}

	return &maxClient{api: api, http: client, token: token}, nil
}

// retryableStatus turns 429 and 5xx responses into transport errors: the MAX
// client retries network errors only and treats any status as final.
type retryableStatus struct {
	next http.RoundTripper
}

func (t retryableStatus) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err == nil && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("MAX ответил %s", resp.Status)
	}

	return resp, err
}

// resolve finds a channel by link (https://max.ru/name), name or id and
// checks that the bot may post there.
func (m *maxClient) resolve(ctx context.Context, ref string) (maxChat, error) {
	var (
		chat model.Chat
		err  error
	)
	if id, parseErr := strconv.ParseInt(ref, 10, 64); parseErr == nil {
		chat, err = m.api.Chats.GetChat(ctx, id)
	} else {
		err = m.get(ctx, "/chats/"+url.PathEscape(channelName(ref, "max.ru")), &chat)
	}
	if err != nil {
		return maxChat{}, fmt.Errorf("канал MAX %q не найден: %w. Бот должен быть администратором канала; сейчас он состоит в: %s",
			ref, err, m.knownChannels(ctx))
	}

	c := maxChat{id: chat.ChatID, title: chat.Title}

	return c, m.checkAdmin(ctx, c)
}

// checkAdmin checks that the bot can still post to the channel.
func (m *maxClient) checkAdmin(ctx context.Context, chat maxChat) error {
	member, err := m.api.Chats.GetMembership(ctx, chat.id)
	if err != nil {
		return fmt.Errorf("не удалось проверить права бота в канале MAX «%s»: %w", chat.title, err)
	}
	if !member.IsAdmin && !member.IsOwner {
		return fmt.Errorf("бот не администратор канала MAX «%s»: публиковать в канале могут только администраторы", chat.title)
	}

	return nil
}

// knownChannels lists the channels the bot is a member of — a setup hint.
func (m *maxClient) knownChannels(ctx context.Context) string {
	var list struct {
		Chats []model.Chat `json:"chats"`
	}
	if err := m.get(ctx, "/chats?count=100", &list); err != nil {
		return "не удалось получить список (" + err.Error() + ")"
	}

	var channels []string
	for _, c := range list.Chats {
		if c.Type == model.ChatTypeChannel {
			channels = append(channels, fmt.Sprintf("«%s» (id %d)", c.Title, c.ChatID))
		}
	}
	if len(channels) == 0 {
		return "ни одного канала"
	}

	return strings.Join(channels, ", ")
}

// get calls the MAX API endpoints the client library lacks.
func (m *maxClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+maxbot.DefaultHostV2+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set(maxbot.AuthorizationHeader, m.token)

	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))

		return fmt.Errorf("MAX ответил %s: %s", resp.Status, bytes.TrimSpace(body))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func (m *maxClient) upload(ctx context.Context, kind model.UploadType, name string, data []byte) (string, error) {
	return m.api.Upload.Upload(ctx, kind, bytes.NewReader(data), name, int64(len(data)))
}

func (m *maxClient) send(ctx context.Context, chatID int64, html string, attachments []attachment, notify, preview bool) error {
	msg := maxbot.NewMessage().SetChat(chatID)
	if html != "" {
		msg.SetText(html).SetFormat(model.FormatHTML)
	}
	for _, a := range attachments {
		msg.AddAttachByToken(a.token, a.kind)
	}
	if !notify {
		msg.WithoutNotify()
	}
	if !preview {
		msg.SetDisableLinkPreview(true)
	}
	_, err := m.api.Messages.Send(ctx, msg)

	return err
}
