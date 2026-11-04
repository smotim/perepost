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
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
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

func isNumber(s string) bool {
	_, err := strconv.ParseInt(s, 10, 64)

	return err == nil
}

// maxChat is a MAX channel the bot posts to.
type maxChat struct {
	id    int64
	title string
	kind  model.ChatType
}

type maxClient struct {
	api     *maxbot.Api
	http    *http.Client
	publish *http.Client
	baseURL string
	token   string
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

func maxMediaClient() (*http.Client, error) {
	transport, err := maxTransport()
	if err != nil {
		return nil, err
	}
	// Attachment URLs come from posts. Resolve and connect to a public
	// address ourselves, including after redirects.
	transport.Proxy = nil
	transport.DialContext = dialPublicMedia

	return &http.Client{Transport: transport, Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || req.URL.User != nil || len(via) >= 5 {
			return http.ErrUseLastResponse
		}

		return nil
	}}, nil
}

var sharedIPv4 = netip.MustParsePrefix("100.64.0.0/10")

func dialPublicMedia(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 4 * time.Second}
	for _, ip := range ips {
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || sharedIPv4.Contains(ip) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}

	return nil, errors.New("не удалось подключиться к публичному адресу вложения MAX")
}

func newMaxClient(token string) (*maxClient, error) {
	transport, err := maxTransport()
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		// The library default of 30 s is far too short for a video of up to 2 GB
		Timeout:   30 * time.Minute,
		Transport: retryableStatus{next: transport},
	}
	api, err := maxbot.NewApi(token, maxbot.WithHTTPClient(client))
	if err != nil {
		return nil, err
	}

	return &maxClient{
		api: api, http: client, token: token, baseURL: "https://" + maxbot.DefaultHostV2,
		publish: &http.Client{Transport: transport, Timeout: 90 * time.Second, CheckRedirect: noRedirect},
	}, nil
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
		hint := "по ссылке MAX находит не все каналы — надёжнее числовой id: он в адресной строке, если открыть канал на web.max.ru"
		if strings.HasPrefix(ref, "-") || !isNumber(ref) {
			hint += "; и проверьте, что бот — администратор канала"
		} else {
			hint = "у каналов MAX id отрицательный — попробуйте -" + ref
		}

		return maxChat{}, fmt.Errorf("канал MAX %q не найден (%w): %s", ref, err, hint)
	}

	c := maxChat{id: chat.ChatID, title: chat.Title, kind: chat.Type}

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

func (m *maxClient) checkRead(ctx context.Context, chat maxChat) error {
	if chat.kind != model.ChatTypeChannel {
		return fmt.Errorf("«%s» — не канал MAX", chat.title)
	}
	member, err := m.api.Chats.GetMembership(ctx, chat.id)
	if err != nil {
		return fmt.Errorf("не удалось проверить право чтения постов MAX «%s»: %w", chat.title, err)
	}
	if !member.IsOwner && (!member.IsAdmin || !slices.Contains(member.Permissions, "read_all_messages")) {
		return fmt.Errorf("боту нужно право чтения сообщений в канале MAX «%s»", chat.title)
	}

	return nil
}

// get calls the MAX API endpoints the client library lacks.
func (m *maxClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+path, nil)
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

func (m *maxClient) upload(ctx context.Context, kind model.UploadType, name string, file tgFile) (string, error) {
	r, err := file.open()
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()

	return m.api.Upload.Upload(ctx, kind, r, name, file.size)
}

// The SDK retries its POST even after a lost response. Use one HTTP request
// here so that only confirmed rejections can be retried by the delivery book.
func (m *maxClient) send(ctx context.Context, chatID int64, html string, attachments []attachment, notify, preview bool) ([]string, error) {
	msg := model.NewMessageBody{Text: html, Attachments: make([]model.Attachment, 0, len(attachments))}
	if html != "" {
		msg.Format = model.FormatHTML
	}
	for _, a := range attachments {
		msg.Attachments = append(msg.Attachments, model.Attachment{Type: a.kind, Payload: model.Payload{Token: a.token}})
	}
	if !notify {
		msg.Notify = &notify
	}
	query := url.Values{"chat_id": {strconv.FormatInt(chatID, 10)}}
	if !preview {
		query.Set("disable_link_preview", "true")
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, &publishError{message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/messages?"+query.Encode(), bytes.NewReader(payload))
	if err != nil {
		return nil, &publishError{message: "не удалось собрать запрос публикации в MAX"}
	}
	req.Header.Set(maxbot.AuthorizationHeader, m.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.publish.Do(req)
	if err != nil {
		return nil, withoutURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		var apiErr maxbot.Error
		_ = json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(&apiErr)
		message := fmt.Sprintf("MAX ответил %s: %s", resp.Status, apiErr.Error())
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, &publishError{message, rateLimitDelay(resp.Header.Get("Retry-After"))}
		case definitivelyRejected(resp.StatusCode) && apiErr.IsAttachmentNotReady():
			return nil, &publishError{message, 2 * time.Second}
		case definitivelyRejected(resp.StatusCode):
			return nil, &publishError{message: message}
		default:
			return nil, errors.New(message)
		}
	}
	var result model.SendMessageResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("ответ публикации MAX: %w", err)
	}
	if result.Message.Body.Mid == "" {
		return nil, errors.New("MAX не вернул mid")
	}

	return []string{result.Message.Body.Mid}, nil
}
