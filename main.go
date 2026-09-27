// Perepost forwards posts of Telegram channels into MAX channels.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// version is set at build time (-ldflags "-X main.version=1.2.3"). A binary
// built from the module (go install …@v1.2.3) takes it from the build info.
var version = "dev"

const (
	// albumWait is how long to wait for the next part of an album before sending it.
	albumWait = 2 * time.Second
	// shutdownGrace is how long to keep sending already received posts on shutdown.
	shutdownGrace = 50 * time.Second
	// setupTimeout bounds resolving every channel at startup.
	setupTimeout = 5 * time.Minute
)

func main() {
	if info, ok := debug.ReadBuildInfo(); ok && version == "dev" && strings.HasPrefix(info.Main.Version, "v") {
		version = strings.TrimPrefix(info.Main.Version, "v")
	}
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)

		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()})))
	if err := run(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	slog.Info("perepost", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var sources map[int64]*source
	tg, err := bot.New(cfg.telegramToken,
		// One synchronous handler: otherwise posts could get reordered
		bot.WithWorkers(1),
		bot.WithNotAsyncHandlers(),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"channel_post"}),
		bot.WithErrorsHandler(func(err error) { slog.Error("Telegram", "err", err) }),
		bot.WithDefaultHandler(func(_ context.Context, _ *bot.Bot, update *models.Update) {
			m := update.ChannelPost
			if m == nil {
				return
			}
			src, ok := sources[m.Chat.ID]
			if !ok {
				slog.Warn("пост из канала без маршрута пропущен", "chat_id", m.Chat.ID, "title", m.Chat.Title)

				return
			}
			src.posts.add(m)
		}),
	)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к Telegram: %w", err)
	}
	maxc, err := newMaxClient(cfg.maxToken)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к MAX: %w", err)
	}

	setup, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	sources, err = connect(setup, tg, maxc, cfg.routes)
	if err != nil {
		return err
	}
	list := slices.SortedFunc(maps.Values(sources), func(a, b *source) int { return cmp.Compare(a.channel.Title, b.channel.Title) })

	r := &relay{tg: tg, max: maxc, http: &http.Client{Timeout: 5 * time.Minute}, adminID: cfg.adminID}
	// Sending outlives the stop signal: posts already received get delivered
	sending, abort := context.WithCancel(context.Background())
	defer abort()
	var workers sync.WaitGroup
	for _, src := range list {
		workers.Go(func() {
			for p := range src.posts.posts {
				<-p.ready
				r.forward(sending, src, p)
			}
		})
	}

	summary := describe(list)
	slog.Info("пересылаю посты", "routes", summary)
	if err := r.alert(fmt.Sprintf("запущен, версия %s:\n%s", version, summary)); err != nil {
		slog.Warn("написать на TELEGRAM_ADMIN_ID не получилось: откройте бота в Telegram и нажмите «Старт»")
	}
	health := &monitor{relay: r, sources: list, to: destinations(list), heartbeat: cfg.heartbeat, client: &http.Client{Timeout: 30 * time.Second}}
	go health.run(ctx)

	tg.Start(ctx)

	slog.Info("останавливаюсь, досылаю полученные посты")
	for _, src := range list {
		src.posts.close()
	}
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		abort()
		<-done

		return errors.New("не успел дослать посты до остановки")
	}

	return nil
}

// connect resolves every channel of the routes and checks the bot's rights in
// each. A channel met in several routes is resolved once, and a source gets
// the destinations of every route it appears in.
func connect(ctx context.Context, tg *bot.Bot, maxc *maxClient, routes []route) (map[int64]*source, error) {
	if err := checkWebhook(ctx, tg); err != nil {
		return nil, err
	}

	channels := map[string]*models.ChatFullInfo{}
	chats := map[string]maxChat{}
	sources := map[int64]*source{}
	for _, rt := range routes {
		var to []maxChat
		for _, ref := range rt.To {
			chat, ok := chats[ref]
			if !ok {
				var err error
				if chat, err = maxc.resolve(ctx, ref); err != nil {
					return nil, err
				}
				chats[ref] = chat
			}
			to = append(to, chat)
		}

		for _, ref := range rt.From {
			channel, ok := channels[ref]
			if !ok {
				var err error
				if channel, err = useChannel(ctx, tg, ref); err != nil {
					return nil, err
				}
				channels[ref] = channel
			}
			src := sources[channel.ID]
			if src == nil {
				src = &source{channel: channel, posts: newQueue(albumWait)}
				sources[channel.ID] = src
			}
			for _, chat := range to {
				if !slices.ContainsFunc(src.to, func(c maxChat) bool { return c.id == chat.id }) {
					src.to = append(src.to, chat)
				}
			}
		}
	}

	return sources, nil
}

// destinations lists every MAX channel once.
func destinations(sources []*source) []maxChat {
	var all []maxChat
	for _, src := range sources {
		for _, chat := range src.to {
			if !slices.ContainsFunc(all, func(c maxChat) bool { return c.id == chat.id }) {
				all = append(all, chat)
			}
		}
	}

	return all
}

// describe renders the routes for the log and the startup message.
func describe(sources []*source) string {
	lines := make([]string, 0, len(sources))
	for _, src := range sources {
		names := make([]string, 0, len(src.to))
		for _, chat := range src.to {
			names = append(names, "«"+chat.title+"»")
		}
		lines = append(lines, fmt.Sprintf("«%s» → %s", src.channel.Title, strings.Join(names, ", ")))
	}

	return strings.Join(lines, "\n")
}

func logLevel() slog.Level {
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		return slog.LevelDebug
	}

	return slog.LevelInfo
}
