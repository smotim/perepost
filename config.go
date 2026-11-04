package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// config holds everything the bot needs to start. Secrets and monitoring come
// from the environment, routes from a YAML file — or from two variables when
// there is a single pair of channels.
type config struct {
	telegramToken string
	maxToken      string
	adminID       int64  // Telegram user to report failures to; 0 — nobody
	heartbeat     string // URL to POST to after every successful health check
	telegramAPI   string // own Bot API server (--local mode): files up to 2 GB instead of 20 MB
	stateFile     string
	maxWebhookURL string
	maxListen     string
	maxSecret     string
	tgWebhookURL  string
	tgSecret      string
	routes        []route
}

// route sends every post of each "from" channel to each "to" channel.
type route struct {
	Name        string `yaml:"name"`
	Enabled     *bool  `yaml:"enabled"`
	From        refs   `yaml:"from"`
	To          refs   `yaml:"to"`
	reverse     bool
	syncEdits   bool
	syncDeletes bool
}

type platform string

const (
	telegramPlatform platform = "telegram"
	maxPlatform      platform = "max"
)

var validWebhookSecret = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,256}$`)

func (r *route) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("строка %d: нужен объект маршрута", node.Line)
	}
	platforms := [2]platform{telegramPlatform, maxPlatform}
	seen := map[string]bool{}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		if seen[key] {
			return fmt.Errorf("строка %d: поле %q повторяется", value.Line, key)
		}
		seen[key] = true
		var err error
		switch key {
		case "name":
			err = value.Decode(&r.Name)
		case "enabled":
			err = value.Decode(&r.Enabled)
		case "sync_edits":
			err = value.Decode(&r.syncEdits)
		case "sync_deletes":
			err = value.Decode(&r.syncDeletes)
		case "from":
			err = decodeEndpoint(value, &r.From, &platforms[0])
		case "to":
			err = decodeEndpoint(value, &r.To, &platforms[1])
		default:
			return fmt.Errorf("строка %d: неизвестное поле %q", value.Line, key)
		}
		if err != nil {
			return err
		}
	}
	if platforms[0] == platforms[1] {
		return fmt.Errorf("строка %d: from и to должны быть на разных платформах", node.Line)
	}
	r.reverse = platforms[0] == maxPlatform

	return nil
}

func decodeEndpoint(node *yaml.Node, channels *refs, p *platform) error {
	if node.Kind != yaml.MappingNode {
		return node.Decode(channels)
	}
	seenPlatform, seenChannels := false, false
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		switch {
		case key == "platform" && !seenPlatform:
			*p = platform(value.Value)
			seenPlatform = true
		case (key == "channel" || key == "channels") && !seenChannels:
			if err := value.Decode(channels); err != nil {
				return err
			}
			seenChannels = true
		default:
			return fmt.Errorf("строка %d: неизвестное или повторное поле %q", value.Line, key)
		}
	}
	if !seenPlatform || (*p != telegramPlatform && *p != maxPlatform) {
		return fmt.Errorf("строка %d: platform должен быть telegram или max", node.Line)
	}

	return nil
}

// refs is a channel or a list of channels: a link, @name or numeric id.
type refs []string

func (r *refs) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*r = refs{node.Value}

		return nil
	case yaml.SequenceNode:
		var list []string
		if err := node.Decode(&list); err != nil {
			return err
		}
		*r = list

		return nil
	}

	return fmt.Errorf("строка %d: нужен канал или список каналов", node.Line)
}

func loadConfig() (config, error) {
	env := func(key string) string { return strings.TrimSpace(os.Getenv(key)) }

	c := config{
		telegramToken: env("TELEGRAM_BOT_TOKEN"),
		maxToken:      env("MAX_BOT_TOKEN"),
		heartbeat:     env("HEARTBEAT_URL"),
		telegramAPI:   strings.TrimSuffix(env("TELEGRAM_API_URL"), "/"),
		stateFile:     env("STATE_FILE"),
		maxWebhookURL: env("MAX_WEBHOOK_URL"),
		maxListen:     env("MAX_WEBHOOK_LISTEN"),
		maxSecret:     env("MAX_WEBHOOK_SECRET"),
		tgWebhookURL:  env("TELEGRAM_WEBHOOK_URL"),
		tgSecret:      env("TELEGRAM_WEBHOOK_SECRET"),
	}
	var missing []string
	if c.telegramToken == "" {
		missing = append(missing, "TELEGRAM_BOT_TOKEN")
	}
	if c.maxToken == "" {
		missing = append(missing, "MAX_BOT_TOKEN")
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("не заданы переменные окружения: %s", strings.Join(missing, ", "))
	}

	if v := env("TELEGRAM_ADMIN_ID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return c, fmt.Errorf("TELEGRAM_ADMIN_ID — числовой id пользователя Telegram, а не %q", v)
		}
		c.adminID = id
	}
	if u, err := url.Parse(c.heartbeat); c.heartbeat != "" && (err != nil || (u.Scheme != "https" && u.Scheme != "http")) {
		return c, errors.New("HEARTBEAT_URL — адрес вида https://…")
	}
	if u, err := url.Parse(c.telegramAPI); c.telegramAPI != "" && (err != nil || (u.Scheme != "https" && u.Scheme != "http")) {
		return c, errors.New("TELEGRAM_API_URL — адрес сервера Bot API вида http://telegram-bot-api:8081")
	}

	path := env("CONFIG_FILE")
	if path == "" {
		path = "config.yaml"
	}
	routes, err := loadRoutes(path, env("TELEGRAM_CHANNEL"), env("MAX_CHANNEL"))
	c.routes = routes
	if err != nil {
		return c, err
	}
	if c.stateFile == "" {
		c.stateFile = "perepost.sqlite"
	}
	if c.maxListen == "" {
		c.maxListen = cmp.Or(env("WEBHOOK_LISTEN"), "127.0.0.1:8090")
	}
	if c.tgWebhookURL != "" {
		u, err := url.Parse(c.tgWebhookURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" ||
			u.Path == "" || u.Path == "/" || (u.Port() != "" && u.Port() != "443") {
			return c, errors.New("TELEGRAM_WEBHOOK_URL — публичный HTTPS-адрес с отдельным путём на порту 443")
		}
		if !validWebhookSecret.MatchString(c.tgSecret) {
			return c, errors.New("TELEGRAM_WEBHOOK_SECRET — 16–256 символов из A–Z, a–z, 0–9, _ и -")
		}
		if maxURL, err := url.Parse(c.maxWebhookURL); err == nil && maxURL.Path == u.Path {
			return c, errors.New("MAX и Telegram нужны разные пути вебхуков")
		}
	}
	if slices.ContainsFunc(routes, func(r route) bool { return r.reverse }) {
		u, err := url.Parse(c.maxWebhookURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" ||
			(u.Port() != "" && u.Port() != "443") {
			return c, errors.New("MAX_WEBHOOK_URL — публичный HTTPS-адрес вебхука на порту 443 без параметров")
		}
		if !validWebhookSecret.MatchString(c.maxSecret) {
			return c, errors.New("MAX_WEBHOOK_SECRET — 16–256 символов из A–Z, a–z, 0–9, _ и -")
		}
	}

	return c, nil
}

// loadRoutes takes a single pair from TELEGRAM_CHANNEL and MAX_CHANNEL, or all
// routes from the YAML file at path. Both at once is an error: it is unclear
// which one the user meant.
func loadRoutes(path, telegramChannel, maxChannel string) ([]route, error) {
	if telegramChannel != "" || maxChannel != "" {
		if telegramChannel == "" || maxChannel == "" {
			return nil, errors.New("для одной пары каналов задайте и TELEGRAM_CHANNEL, и MAX_CHANNEL")
		}
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("маршруты заданы и в TELEGRAM_CHANNEL/MAX_CHANNEL, и в %s — оставьте что-то одно", path)
		}

		return []route{{From: refs{telegramChannel}, To: refs{maxChannel}}}, nil
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("маршруты не заданы: создайте %s (пример — config.example.yaml) или задайте TELEGRAM_CHANNEL и MAX_CHANNEL", path)
	}
	if err != nil {
		return nil, err
	}

	var file struct {
		Routes []route `yaml:"routes"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true) // a typo like "form:" must not silently drop a route
	if err := decoder.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(file.Routes) == 0 {
		return nil, fmt.Errorf("%s: в routes нет ни одного маршрута", path)
	}
	active := make([]route, 0, len(file.Routes))
	for i, r := range file.Routes {
		if r.Enabled != nil && !*r.Enabled {
			continue
		}
		if r.syncDeletes && !r.reverse {
			return nil, fmt.Errorf("%s: маршрут %d: sync_deletes доступен для источников MAX", path, i+1)
		}
		for _, list := range []refs{r.From, r.To} {
			if len(list) == 0 || slices.ContainsFunc(list, func(ref string) bool { return strings.TrimSpace(ref) == "" }) {
				return nil, fmt.Errorf("%s: у маршрута %d нужны непустые from и to", path, i+1)
			}
		}
		active = append(active, r)
	}
	if len(active) == 0 {
		return nil, fmt.Errorf("%s: нет включённых маршрутов", path)
	}

	return active, nil
}
