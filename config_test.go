package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestSinglePairFromEnvironment(t *testing.T) {
	routes, err := loadRoutes(filepath.Join(t.TempDir(), "missing.yaml"), "@news", refs{"https://max.ru/news", "vk:news"})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || !slices.Equal(routes[0].From, refs{"@news"}) || !slices.Equal(routes[0].To, refs{"https://max.ru/news", "vk:news"}) {
		t.Fatalf("routes %+v", routes)
	}
}

func TestRoutesFromFileAcceptChannelOrList(t *testing.T) {
	path := writeConfig(t, `
routes:
  - from: https://t.me/a
    to: https://max.ru/x
  - from: [https://t.me/b, -1001234567890]
    to:
      - https://max.ru/y
      - -71234567
`)
	routes, err := loadRoutes(path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 ||
		!slices.Equal(routes[1].From, refs{"https://t.me/b", "-1001234567890"}) ||
		!slices.Equal(routes[1].To, refs{"https://max.ru/y", "-71234567"}) {
		t.Fatalf("routes %+v", routes)
	}
}

func TestConfigMistakesAreReported(t *testing.T) {
	cases := map[string]struct {
		yaml, telegram, target, want string
	}{
		"typo in a key":   {yaml: "routes:\n  - form: a\n    to: b\n", want: "form"},
		"empty route":     {yaml: "routes:\n  - from: a\n", want: "непустые from и to"},
		"no routes":       {yaml: "# nothing yet\n", want: "нет ни одного маршрута"},
		"both env & file": {yaml: "routes: []\n", telegram: "@a", target: "b", want: "оставьте что-то одно"},
		"no target":       {telegram: "@a", want: "хотя бы одно из MAX_CHANNEL, VK_GROUP"},
		"nothing at all":  {want: "маршруты не заданы"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing.yaml")
			if c.yaml != "" {
				path = writeConfig(t, c.yaml)
			}
			var targets refs
			if c.target != "" {
				targets = refs{c.target}
			}
			_, err := loadRoutes(path, c.telegram, targets)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
		})
	}
}

func TestTokensAreRequiredOnlyForUsedPlatforms(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CONFIG_FILE", filepath.Join(dir, "missing.yaml"))
	t.Setenv("TELEGRAM_BOT_TOKEN", "1:x")
	t.Setenv("TELEGRAM_CHANNEL", "@news")
	t.Setenv("MAX_CHANNEL", "")
	t.Setenv("MAX_BOT_TOKEN", "")
	t.Setenv("VK_GROUP", "my_group")
	t.Setenv("VK_TOKEN", "")

	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "VK_TOKEN") || strings.Contains(err.Error(), "MAX_BOT_TOKEN") {
		t.Fatalf("err %v", err)
	}

	t.Setenv("VK_TOKEN", "vk1.a.token")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.routes[0].To, refs{"vk:my_group"}) {
		t.Fatalf("routes %+v", cfg.routes)
	}
}
