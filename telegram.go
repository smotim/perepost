package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// tgDownloadLimit is the largest file the Bot API lets a bot download.
const tgDownloadLimit = 20 << 20

var errTooBig = errors.New("файл больше 20 МБ: Bot API такие не отдаёт")

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

// downloadFile downloads a whole file: MAX needs its size up front.
func downloadFile(ctx context.Context, tg *bot.Bot, client *http.Client, fileID string) ([]byte, error) {
	file, err := tg.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tg.FileDownloadLink(file), nil)
	if err != nil {
		return nil, errors.New("не удалось собрать запрос на скачивание")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("скачивание файла: %w", withoutURL(err)) // the URL contains the bot token
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("скачивание файла: Telegram ответил %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, tgDownloadLimit+1))
	if err != nil {
		return nil, fmt.Errorf("скачивание файла: %w", err)
	}
	if len(data) > tgDownloadLimit {
		return nil, errTooBig
	}

	return data, nil
}

// withoutURL strips the request URL from an error: it may hold a token or secret.
func withoutURL(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Err
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
