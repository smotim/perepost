// Perepost forwards channel posts between Telegram and MAX.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
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
	// maxPostFailures is how many crashes in a row a post gets before it is skipped.
	maxPostFailures = 3
)

func main() {
	if info, ok := debug.ReadBuildInfo(); ok && version == "dev" && strings.HasPrefix(info.Main.Version, "v") {
		version = strings.TrimPrefix(info.Main.Version, "v")
	}
	showVersion := flag.Bool("version", false, "print the version and exit")
	resolve := flag.Bool("resolve-delivery", false, "finish manual recovery of a partial or uncertain delivery: SOURCE POST DESTINATION none|ID [ID...]")
	resolveUncertain := flag.Bool("resolve-uncertain", false, "alias for -resolve-delivery")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)

		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()})))
	start := run
	if *resolve || *resolveUncertain {
		start = func() error { return resolveDelivery(flag.Args()) }
	}
	if err := start(); err != nil {
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

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(signalContext)
	defer cancel()

	db, err := openStore(ctx, cfg.stateFile)
	if err != nil {
		return fmt.Errorf("не удалось открыть STATE_FILE: %w", err)
	}
	defer db.close()
	if err := db.unresolved(ctx); err != nil {
		return err
	}

	var options []bot.Option
	tgTransport := pacedTransport(http.DefaultTransport, time.Second)
	options = append(options, bot.WithHTTPClient(time.Minute, &http.Client{Transport: tgTransport, Timeout: time.Minute}))
	fileLimit := int64(tgDownloadLimit)
	apiURL, maxFileLimit, maxPostLimit := "https://api.telegram.org", int64(50<<20), int64(200<<20)
	if cfg.telegramAPI != "" {
		options = append(options, bot.WithServerURL(cfg.telegramAPI))
		apiURL = cfg.telegramAPI
		fileLimit = 0
		maxFileLimit, maxPostLimit = 2_000_000_000, 2_000_000_000
	}
	tg, err := bot.New(cfg.telegramToken, options...)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к Telegram: %w", err)
	}
	maxc, err := newMaxClient(cfg.maxToken)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к MAX: %w", err)
	}

	setup, finishSetup := context.WithTimeout(ctx, setupTimeout)
	defer finishSetup()
	channels, err := connect(setup, tg, maxc, cfg.routes)
	if err != nil {
		return err
	}
	list := slices.SortedFunc(maps.Values(channels.telegram), func(a, b *source) int { return cmp.Compare(a.channel.Title, b.channel.Title) })
	maxList := slices.SortedFunc(maps.Values(channels.max), func(a, b *maxSource) int { return cmp.Compare(a.channel.title, b.channel.title) })
	tgEvents, maxEvents := webhookEvents(cfg.routes)
	if len(list) > 0 && cfg.tgWebhookURL == "" {
		if err := configureTelegramWebhook(setup, tg, "", "", nil, false); err != nil {
			return err
		}
	}
	r := &relay{
		tg: tg, max: maxc, http: &http.Client{Timeout: 5 * time.Minute}, adminID: cfg.adminID, fileLimit: fileLimit,
		tgAPI:        telegramAPI{baseURL: apiURL, token: cfg.telegramToken, http: &http.Client{Transport: tgTransport, Timeout: 15 * time.Minute, CheckRedirect: noRedirect}},
		maxFileLimit: maxFileLimit, maxPostLimit: maxPostLimit, mediaSlot: make(chan struct{}, 1),
	}
	var server *http.Server
	var listener net.Listener
	handlers := map[string]http.Handler{}
	if len(maxList) > 0 {
		r.mediaDir = cfg.stateFile + ".media"
		if err := prepareMediaDir(r.mediaDir); err != nil {
			return err
		}
		r.maxMediaHTTP, err = maxMediaClient()
		if err != nil {
			return err
		}
		u, _ := url.Parse(cfg.maxWebhookURL) // validated by loadConfig
		handlers[cmp.Or(u.EscapedPath(), "/")] = maxWebhook(cfg.maxSecret, db, channels.max)
	}
	if len(list) > 0 && cfg.tgWebhookURL != "" {
		u, _ := url.Parse(cfg.tgWebhookURL)
		handlers[u.EscapedPath()] = telegramWebhook(cfg.tgSecret, db, channels.telegram)
	}
	if len(handlers) > 0 {
		server = &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				handler := handlers[req.URL.EscapedPath()]
				if handler == nil {
					http.NotFound(w, req)

					return
				}
				handler.ServeHTTP(w, req)
			}),
			ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
			WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second,
		}
		listener, err = (&net.ListenConfig{}).Listen(setup, "tcp", cfg.maxListen)
		if err != nil {
			return fmt.Errorf("не удалось открыть WEBHOOK_LISTEN: %w", err)
		}
		defer func() { _ = server.Close() }()
	}

	var failure error
	var failOnce sync.Once
	fail := func(err error) {
		if err != nil {
			failOnce.Do(func() {
				failure = err
				cancel()
			})
		}
	}
	var inputs sync.WaitGroup
	if server != nil {
		inputs.Go(func() {
			if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				fail(err)
			}
		})
		if len(maxList) > 0 {
			if err := maxc.webhook(setup, cfg.maxWebhookURL, cfg.maxSecret, maxEvents, true); err != nil {
				return err
			}
		}
		if len(list) > 0 && cfg.tgWebhookURL != "" {
			if err := configureTelegramWebhook(setup, tg, cfg.tgWebhookURL, cfg.tgSecret, tgEvents, true); err != nil {
				return err
			}
		}
	}
	finishSetup()

	// Sending outlives the stop signal: posts already received get delivered
	sending, abort := context.WithCancel(context.Background())
	defer abort()
	intakeDone := make(chan struct{})
	var workers sync.WaitGroup
	for _, src := range list {
		workers.Go(func() {
			key := channelKey{telegramPlatform, src.channel.ID}
			fail(process(sending, db, key, intakeDone, r.alert, func(p pendingPost, book *deliveryBook) {
				r.forwardTelegramPost(sending, src, p, book)
			}))
		})
	}
	for _, src := range maxList {
		workers.Go(func() {
			key := channelKey{maxPlatform, src.channel.id}
			fail(process(sending, db, key, intakeDone, r.alert, func(p pendingPost, book *deliveryBook) {
				r.forwardMaxPost(sending, src, p, book)
			}))
		})
	}

	summary := describe(list, maxList)
	slog.Info("пересылаю посты", "routes", summary)
	if err := r.alert(ctx, fmt.Sprintf("запущен, версия %s:\n%s", version, summary)); err != nil {
		slog.Warn("написать на TELEGRAM_ADMIN_ID не получилось: откройте бота в Telegram и нажмите «Старт»")
	}
	health := &monitor{
		relay: r, sources: list, to: destinations(list), maxSources: maxList, heartbeat: cfg.heartbeat,
		client: &http.Client{Timeout: 30 * time.Second}, db: db, maxWebhookURL: cfg.maxWebhookURL,
		tgWebhookURL: cfg.tgWebhookURL, tgEvents: tgEvents, maxEvents: maxEvents,
	}
	health.lastPoll.Store(time.Now().UnixNano())
	var monitoring sync.WaitGroup
	monitoring.Go(func() { health.run(ctx) })
	if len(list) > 0 && cfg.tgWebhookURL == "" {
		inputs.Go(func() {
			if err := r.tgAPI.poll(ctx, db, channels.telegram, tgEvents, &health.lastPoll); ctx.Err() == nil {
				fail(err)
			}
		})
	}
	<-ctx.Done()
	stop() // A second interrupt must stop the process even while it drains.

	slog.Info("останавливаюсь, досылаю полученные посты")
	done := make(chan struct{})
	go func() {
		if server != nil {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdown); err != nil {
				_ = server.Close()
			}
		}
		inputs.Wait()
		close(intakeDone)
		workers.Wait()
		monitoring.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		abort()
		<-done

		slog.Info("оставшиеся посты сохранены в очереди")
	}

	return failure
}

func (r *relay) forwardTelegramPost(ctx context.Context, src *source, p pendingPost, book *deliveryBook) {
	if strings.HasPrefix(p.key, "change:") {
		r.editFromTelegram(ctx, src, p, book)

		return
	}
	if strings.HasPrefix(p.key, "album:") && len(p.payloads) > 0 {
		if book.err = book.store.keepAlbum(ctx, book.source, p.key, p.payloads); book.err != nil {
			return
		}
		// The first attempt freezes the album. A resumed delivery must use the
		// same parts even if more updates arrived before the next process started.
		p.payloads, book.err = book.store.albumPayloads(ctx, book.source, p.key)
		if book.err != nil {
			return
		}
	}
	messages, err := telegramMessages(p.payloads)
	if err != nil {
		slog.Warn("пост Telegram разобран не полностью", "channel", book.source, "post", p.key, "err", err)
		action := "переносим доступную часть"
		if len(messages) == 0 {
			action = "пост пропущен"
		}
		_ = r.alert(ctx, fmt.Sprintf("пост %s из %s разобран не полностью, %s: %v", p.key, book.source, action, err))
	}
	if len(messages) > 0 {
		r.forward(ctx, src, &post{messages}, book)
	}
}

func (r *relay) forwardMaxPost(ctx context.Context, src *maxSource, p pendingPost, book *deliveryBook) {
	if strings.HasPrefix(p.key, "change:") {
		r.changeFromMax(ctx, src, p, book)

		return
	}
	if len(p.payloads) == 0 {
		return
	}
	var m maxMessage
	if err := json.Unmarshal(p.payloads[0], &m); err != nil {
		slog.Warn("пост MAX разобран не полностью", "channel", book.source, "post", p.key, "err", err)
		_, partial := errors.AsType[*json.UnmarshalTypeError](err)
		action := "переносим доступные поля"
		if !partial {
			action = "пост пропущен"
		}
		_ = r.alert(ctx, fmt.Sprintf("пост %s из %s разобран не полностью, %s: %v", p.key, book.source, action, err))
		if !partial {
			return
		}
	}
	r.forwardMax(ctx, src, m, book)
}

// process delivers the posts of one source in order. A crash while handling a
// post stops the service, so the restart turns its in-flight publications into
// uncertain ones; after maxPostFailures crashes in a row the post is skipped.
func process(ctx context.Context, db *store, source channelKey, intakeDone <-chan struct{}, alert func(context.Context, string) error, forward func(pendingPost, *deliveryBook)) (err error) {
	defer func() {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			err = nil
		}
	}()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for ctx.Err() == nil {
		now := time.Now()
		draining := false
		select {
		case <-intakeDone:
			draining = true
		default:
		}
		post, err := db.next(ctx, source, now)
		if err != nil {
			return err
		}
		if post.row != 0 {
			if source.platform == telegramPlatform && strings.HasPrefix(post.key, "album:") {
				moved, err := db.splitLateAlbum(ctx, source, post)
				if err != nil {
					return err
				}
				if moved {
					continue
				}
			}
			failures, err := db.failures(ctx, source, post.key)
			if err != nil {
				return err
			}
			if failures >= maxPostFailures {
				slog.Error("пост пропущен после повторных сбоев", "channel", source, "post", post.key, "failures", failures)
				_ = alert(ctx, fmt.Sprintf("пост %s из %s пропущен: обработка падала %d раза подряд", post.key, source, failures))
				if err := db.complete(ctx, source, post); err != nil {
					return err
				}

				continue
			}
			book := &deliveryBook{store: db, source: source, post: post.key}
			if crash := handle(forward, post, book); crash != nil {
				return crashed(ctx, db, source, post.key, crash, alert)
			}
			if book.err != nil {
				return book.err
			}
			if ctx.Err() != nil {
				break
			}
			if err := db.complete(ctx, source, post); err != nil {
				return err
			}

			continue
		}
		if draining {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		case <-intakeDone:
		}
	}

	return nil
}

// crash is a panic caught while handling a post, with the stack it came from.
type crash struct {
	value any
	stack []byte
}

// handle runs forward and turns a panic into a crash: every handler, in both
// directions, gets the same treatment.
func handle(forward func(pendingPost, *deliveryBook), p pendingPost, book *deliveryBook) (c *crash) {
	defer func() {
		if v := recover(); v != nil {
			c = &crash{v, debug.Stack()}
		}
	}()
	forward(p, book)

	return nil
}

// crashed counts the crash, tells the admin and returns the error that stops the service.
func crashed(ctx context.Context, db *store, source channelKey, post string, c *crash, alert func(context.Context, string) error) error {
	slog.Error("сбой при обработке поста", "channel", source, "post", post, "panic", c.value, "stack", string(c.stack))
	// Shutdown may already be cancelling ctx; the count must still be saved.
	saving, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	count, err := db.recordFailure(saving, source, post)
	if err != nil {
		slog.Error("не удалось записать сбой поста", "channel", source, "post", post, "err", err)
	}
	_ = alert(ctx, fmt.Sprintf("сбой при обработке поста %s из %s (%d из %d, затем пост будет пропущен): %v",
		post, source, count, maxPostFailures, c.value))

	return fmt.Errorf("обработка поста %s из %s прервана: %v", post, source, c.value)
}

func resolveDelivery(args []string) error {
	if len(args) < 4 || (args[3] == "none" && len(args) != 4) {
		return errors.New("укажите SOURCE POST DESTINATION и none или id опубликованных сообщений")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := openStore(ctx, cmp.Or(os.Getenv("STATE_FILE"), "perepost.sqlite"))
	if err != nil {
		return err
	}
	defer db.close()
	ids := args[3:]
	if ids[0] == "none" {
		ids = nil
	}

	return db.resolveDelivery(ctx, args[0], args[1], args[2], ids)
}

func prepareMediaDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "download-") {
			if err := os.Remove(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
	}

	return nil
}

type sources struct {
	telegram map[int64]*source
	max      map[int64]*maxSource
}

// connect resolves every channel of the routes and checks the bot's rights in
// each. A channel met in several routes is resolved once, and a source gets
// the destinations of every route it appears in.
func connect(ctx context.Context, tg *bot.Bot, maxc *maxClient, routes []route) (sources, error) {
	telegram := map[string]*models.ChatFullInfo{}
	maximum := map[string]maxChat{}
	getTelegram := func(ref string) (*models.ChatFullInfo, error) {
		if chat := telegram[ref]; chat != nil {
			return chat, nil
		}
		chat, err := useChannel(ctx, tg, ref)
		telegram[ref] = chat

		return chat, err
	}
	getMax := func(ref string) (maxChat, error) {
		if chat, ok := maximum[ref]; ok {
			return chat, nil
		}
		chat, err := maxc.resolve(ctx, ref)
		maximum[ref] = chat

		return chat, err
	}
	result := sources{telegram: map[int64]*source{}, max: map[int64]*maxSource{}}
	canPost := map[int64]bool{}
	canPostMax := map[int64]bool{}
	for _, rt := range routes {
		if rt.reverse {
			var to []*models.ChatFullInfo
			for _, ref := range rt.To {
				chat, err := getTelegram(ref)
				if err != nil {
					return sources{}, err
				}
				if !canPost[chat.ID] {
					if err := checkTelegramPost(ctx, tg, chat); err != nil {
						return sources{}, err
					}
					canPost[chat.ID] = true
				}
				to = append(to, chat)
			}
			for _, ref := range rt.From {
				chat, err := getMax(ref)
				if err != nil {
					return sources{}, err
				}
				src := result.max[chat.id]
				if src == nil {
					if err := maxc.checkRead(ctx, chat); err != nil {
						return sources{}, err
					}
					src = &maxSource{channel: chat, changes: map[int64]syncOptions{}}
					result.max[chat.id] = src
				}
				for _, dst := range to {
					addSyncOptions(src.changes, dst.ID, rt)
					if !slices.ContainsFunc(src.to, func(c *models.ChatFullInfo) bool { return c.ID == dst.ID }) {
						src.to = append(src.to, dst)
					}
				}
			}

			continue
		}
		var to []maxChat
		for _, ref := range rt.To {
			chat, err := getMax(ref)
			if err != nil {
				return sources{}, err
			}
			if !canPostMax[chat.id] {
				if err := maxc.checkAdmin(ctx, chat); err != nil {
					return sources{}, err
				}
				canPostMax[chat.id] = true
			}
			to = append(to, chat)
		}

		for _, ref := range rt.From {
			channel, err := getTelegram(ref)
			if err != nil {
				return sources{}, err
			}
			src := result.telegram[channel.ID]
			if src == nil {
				src = &source{channel: channel, changes: map[int64]syncOptions{}}
				result.telegram[channel.ID] = src
			}
			for _, chat := range to {
				addSyncOptions(src.changes, chat.id, rt)
				if !slices.ContainsFunc(src.to, func(c maxChat) bool { return c.id == chat.id }) {
					src.to = append(src.to, chat)
				}
			}
		}
	}

	return result, nil
}

func webhookEvents(routes []route) (telegram, max []string) {
	telegram = []string{"channel_post"}
	max = []string{maxEvent}
	if slices.ContainsFunc(routes, func(r route) bool { return !r.reverse && r.syncEdits }) {
		telegram = append(telegram, telegramEdit)
	}
	if slices.ContainsFunc(routes, func(r route) bool { return r.reverse && r.syncEdits }) {
		max = append(max, maxEdit)
	}
	if slices.ContainsFunc(routes, func(r route) bool { return r.reverse && r.syncDeletes }) {
		max = append(max, maxDelete)
	}

	return telegram, max
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
func describe(sources []*source, maxSources []*maxSource) string {
	lines := make([]string, 0, len(sources)+len(maxSources))
	for _, src := range sources {
		names := make([]string, 0, len(src.to))
		for _, chat := range src.to {
			names = append(names, "«"+chat.title+"»")
		}
		lines = append(lines, fmt.Sprintf("Telegram «%s» → MAX %s", src.channel.Title, strings.Join(names, ", ")))
	}
	for _, src := range maxSources {
		names := make([]string, 0, len(src.to))
		for _, dst := range src.to {
			names = append(names, "«"+dst.Title+"»")
		}
		lines = append(lines, fmt.Sprintf("MAX «%s» → Telegram %s", src.channel.title, strings.Join(names, ", ")))
	}

	return strings.Join(lines, "\n")
}

func logLevel() slog.Level {
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		return slog.LevelDebug
	}

	return slog.LevelInfo
}
