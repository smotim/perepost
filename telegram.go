package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// tgDownloadLimit is the largest file the cloud Bot API lets a bot download.
// A local Bot API server (TELEGRAM_API_URL, --local mode) has no such limit.
const tgDownloadLimit = 20 << 20

// errNotOnVolume means the local server's file is not where it said: the
// volume is not shared with perepost. Retrying will not help.
var errNotOnVolume = errors.New("файл от сервера Bot API не найден на общем томе — у perepost и telegram-bot-api должен быть один том по одному пути")

var errTooBig = errors.New("файл больше 20 МБ: облачный Bot API такие не отдаёт — поможет свой сервер Bot API (TELEGRAM_API_URL, см. README)")

// tgFile is a downloaded Telegram file: in memory from the cloud Bot API, or
// on a volume shared with a local Bot API server, which answers getFile with
// an absolute path instead of a download link. The path contains the bot
// token, so it never goes into errors or logs.
type tgFile struct {
	data []byte
	path string
	size int64
}

// open is called for every upload attempt: a file on disk is streamed, not
// loaded into memory — a local server hands out files up to 2 GB.
func (f tgFile) open() (io.ReadCloser, error) {
	if f.path == "" {
		return io.NopCloser(bytes.NewReader(f.data)), nil
	}
	file, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("чтение скачанного файла: %w", withoutPath(err))
	}

	return file, nil
}

// remove deletes a file the local server downloaded, so the volume does not
// fill up with videos. Telegram downloads it again if it is ever requested.
func (f tgFile) remove() {
	if f.path == "" {
		return
	}
	if err := os.Remove(f.path); err != nil {
		slog.Warn("не удалось удалить скачанный файл", "err", withoutPath(err))
	}
}

// checkWebhook refuses a bot that has a webhook: Telegram does not hand its
// updates to getUpdates, and deleting the webhook would break whoever set it.
func checkWebhook(ctx context.Context, tg *bot.Bot) error {
	hook, err := tg.GetWebhookInfo(ctx)
	if err != nil {
		return fmt.Errorf("не удалось проверить вебхук Telegram-бота: %w", err)
	}
	if hook.URL == "" {
		return nil
	}

	host := "другой адрес" // not the URL itself: it may contain a token
	if u, err := url.Parse(hook.URL); err == nil {
		host = u.Host
	}

	return fmt.Errorf("у Telegram-бота настроен вебхук на %s — похоже, токен уже занят другим сервисом. "+
		"Заведите для пересылки отдельного бота", host)
}

// useChannel finds a channel by link, @name or numeric id and checks that the
// bot receives its posts.
func useChannel(ctx context.Context, tg *bot.Bot, ref string) (*models.ChatFullInfo, error) {
	var chatID any = "@" + channelName(ref, "t.me")
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		chatID = id
	}
	chat, err := tg.GetChat(ctx, &bot.GetChatParams{ChatID: chatID})
	if err != nil {
		return nil, fmt.Errorf("канал Telegram %q не найден (у закрытого канала укажите числовой id): %w", ref, err)
	}
	if chat.Type != models.ChatTypeChannel {
		return nil, fmt.Errorf("%q — не канал", ref)
	}

	return chat, checkTelegramAdmin(ctx, tg, chat)
}

// checkTelegramAdmin checks that the bot is an administrator of the channel:
// Telegram sends channel posts to administrators only. A bot removed from the
// admins gets no error — posts just stop coming.
func checkTelegramAdmin(ctx context.Context, tg *bot.Bot, chat *models.ChatFullInfo) error {
	member, err := tg.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: chat.ID, UserID: tg.ID()})
	if err != nil {
		return fmt.Errorf("не удалось проверить права бота в канале Telegram «%s»: %w", chat.Title, err)
	}
	if member.Type != models.ChatMemberTypeAdministrator {
		return fmt.Errorf("бот не администратор канала Telegram «%s»: посты канала Telegram присылает только администраторам", chat.Title)
	}

	return nil
}

// downloadFile fetches a file: a local Bot API server has already put it on
// the shared volume, the cloud one is downloaded whole — MAX needs the size up front.
func downloadFile(ctx context.Context, tg *bot.Bot, client *http.Client, fileID string) (tgFile, error) {
	file, err := tg.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return tgFile{}, err
	}
	if filepath.IsAbs(file.FilePath) {
		info, err := os.Stat(file.FilePath)
		if err != nil {
			return tgFile{}, fmt.Errorf("%w (%w)", errNotOnVolume, withoutPath(err))
		}

		return tgFile{path: file.FilePath, size: info.Size()}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tg.FileDownloadLink(file), nil)
	if err != nil {
		return tgFile{}, errors.New("не удалось собрать запрос на скачивание")
	}
	resp, err := client.Do(req)
	if err != nil {
		return tgFile{}, fmt.Errorf("скачивание файла: %w", withoutURL(err)) // the URL contains the bot token
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return tgFile{}, fmt.Errorf("скачивание файла: Telegram ответил %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, tgDownloadLimit+1))
	if err != nil {
		return tgFile{}, fmt.Errorf("скачивание файла: %w", err)
	}
	if len(data) > tgDownloadLimit {
		return tgFile{}, errTooBig
	}

	return tgFile{data: data, size: int64(len(data))}, nil
}

// withoutURL strips the request URL from an error: it may hold a token or secret.
func withoutURL(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Err
	}

	return err
}

// withoutPath strips the file path from an error: a local server's paths
// contain the bot token.
func withoutPath(err error) error {
	if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
		return fmt.Errorf("%s: %w", pathErr.Op, pathErr.Err)
	}

	return err
}

// channelName extracts a channel name from a link (https://t.me/name,
// t.me/name) or from @name.
func channelName(ref, host string) string {
	name := strings.TrimSpace(ref)
	name = strings.TrimPrefix(strings.TrimPrefix(name, "https://"), "http://")
	name = strings.TrimPrefix(strings.TrimPrefix(name, "www."), host+"/")
	name = strings.TrimSuffix(name, "/")

	return strings.TrimPrefix(name, "@")
}
