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
	routes, err := loadRoutes(filepath.Join(t.TempDir(), "missing.yaml"), "@news", "https://max.ru/news")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || !slices.Equal(routes[0].From, refs{"@news"}) || !slices.Equal(routes[0].To, refs{"https://max.ru/news"}) {
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
	routes, err := loadRoutes(path, "", "")
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
		yaml, telegram, max, want string
	}{
		"typo in a key":   {yaml: "routes:\n  - form: a\n    to: b\n", want: "form"},
		"empty route":     {yaml: "routes:\n  - from: a\n", want: "непустые from и to"},
		"no routes":       {yaml: "# nothing yet\n", want: "нет ни одного маршрута"},
		"both env & file": {yaml: "routes: []\n", telegram: "@a", max: "b", want: "оставьте что-то одно"},
		"half of a pair":  {telegram: "@a", want: "и TELEGRAM_CHANNEL, и MAX_CHANNEL"},
		"nothing at all":  {want: "маршруты не заданы"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing.yaml")
			if c.yaml != "" {
				path = writeConfig(t, c.yaml)
			}
			_, err := loadRoutes(path, c.telegram, c.max)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
		})
	}
}
