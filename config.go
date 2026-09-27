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
	adminID       int64  // Telegram user to report failures to; 0 — nobody
	heartbeat     string // URL to POST to after every successful health check
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

	c := config{telegramToken: env("TELEGRAM_BOT_TOKEN"), maxToken: env("MAX_BOT_TOKEN"), heartbeat: env("HEARTBEAT_URL")}
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

	path := env("CONFIG_FILE")
	if path == "" {
		path = "config.yaml"
	}
	routes, err := loadRoutes(path, env("TELEGRAM_CHANNEL"), env("MAX_CHANNEL"))
	c.routes = routes

	return c, err
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
	for i, r := range file.Routes {
		for _, list := range []refs{r.From, r.To} {
			if len(list) == 0 || slices.ContainsFunc(list, func(ref string) bool { return strings.TrimSpace(ref) == "" }) {
				return nil, fmt.Errorf("%s: у маршрута %d нужны непустые from и to", path, i+1)
			}
		}
	}

	return file.Routes, nil
}
