package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
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
	vkToken       string // user token of a community editor: VK has no wall posting for community tokens
	adminID       int64  // Telegram user to report failures to; 0 — nobody
	heartbeat     string // URL to POST to after every successful health check
	telegramAPI   string // own Bot API server (--local mode): files up to 2 GB instead of 20 MB
	routes        []route
}

// route sends every post of each "from" channel to each "to" channel.
type route struct {
	From refs `yaml:"from"`
	To   refs `yaml:"to"`
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
		vkToken:       env("VK_TOKEN"),
		heartbeat:     env("HEARTBEAT_URL"),
		telegramAPI:   strings.TrimSuffix(env("TELEGRAM_API_URL"), "/"),
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
	var targets refs
	if v := env("MAX_CHANNEL"); v != "" {
		targets = append(targets, v)
	}
	if v := env("VK_GROUP"); v != "" {
		if !isVK(v) {
			v = "vk:" + v
		}
		targets = append(targets, v)
	}
	routes, err := loadRoutes(path, env("TELEGRAM_CHANNEL"), targets)
	if err != nil {
		return c, err
	}
	c.routes = routes

	// A token is needed only for the platforms the routes lead to
	var needMax, needVK bool
	for _, r := range routes {
		for _, ref := range r.To {
			if isVK(ref) {
				needVK = true
			} else {
				needMax = true
			}
		}
	}
	var missing []string
	for _, v := range []struct {
		key    string
		value  string
		needed bool
	}{
		{"TELEGRAM_BOT_TOKEN", c.telegramToken, true},
		{"MAX_BOT_TOKEN", c.maxToken, needMax},
		{"VK_TOKEN", c.vkToken, needVK},
	} {
		if v.needed && v.value == "" {
			missing = append(missing, v.key)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("не заданы переменные окружения: %s", strings.Join(missing, ", "))
	}

	return c, nil
}

// loadRoutes takes a single channel from TELEGRAM_CHANNEL with its targets
// from MAX_CHANNEL and VK_GROUP, or all routes from the YAML file at path. Both
// at once is an error: it is unclear which one the user meant.
func loadRoutes(path, telegramChannel string, targets refs) ([]route, error) {
	if telegramChannel != "" || len(targets) > 0 {
		if telegramChannel == "" || len(targets) == 0 {
			return nil, errors.New("для одного канала задайте TELEGRAM_CHANNEL и хотя бы одно из MAX_CHANNEL, VK_GROUP")
		}
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("маршруты заданы и в переменных окружения, и в %s — оставьте что-то одно", path)
		}

		return []route{{From: refs{telegramChannel}, To: targets}}, nil
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("маршруты не заданы: создайте %s (пример — config.example.yaml) или задайте TELEGRAM_CHANNEL и MAX_CHANNEL / VK_GROUP", path)
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
	for i, r := range file.Routes {
		for _, list := range []refs{r.From, r.To} {
			if len(list) == 0 || slices.ContainsFunc(list, func(ref string) bool { return strings.TrimSpace(ref) == "" }) {
				return nil, fmt.Errorf("%s: у маршрута %d нужны непустые from и to", path, i+1)
			}
		}
	}

	return file.Routes, nil
}
