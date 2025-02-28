package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	telegram "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"github.com/gptscript-ai/go-gptscript"
)

func exitError(err error) {
	fmt.Printf("telegram bot tool failed: %v\n", err)
	os.Exit(1)
}

var (
	allowedUserIDs   = map[int64]struct{}{}
	allowedUserNames = map[string]struct{}{}
	messageQueue     = make(chan Message, 100)
)

func isAuthorized(user *telegram.User) bool {

	if user == nil {
		return false
	}

	if _, ok := allowedUserIDs[user.ID]; ok {
		return true
	}
	if _, ok := allowedUserNames[user.UserName]; ok {
		return true
	}
	return false
}

type Message struct {
	User     string `json:"user,omitempty"`
	ChatID   string `json:"chatId"`
	MsgID    string `json:"msgId,omitempty"`
	Text     string `json:"text"`
	VoiceURL string `json:"voiceURL,omitempty"`
	ImageURL string `json:"imageURL,omitempty"`
	FileExt  string `json:"fileExt,omitempty"`
}

func uploadToWs(ctx context.Context, gClient *gptscript.GPTScript, url string, ext string) (string, error) {
	path := uuid.NewString() + ext

	resp, err := http.Get(url)
	if err != nil {
		return path, err
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return path, err
	}
	resp.Body.Close()

	err = gClient.WriteFileInWorkspace(ctx, path, b)
	if err != nil {
		return path, err
	}

	return path, nil

}

func main() {
	bot, err := telegram.NewBotAPI(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if err != nil {
		exitError(fmt.Errorf("failed to create bot: %w", err))
	}

	gClient, err := gptscript.NewGPTScript()
	if err != nil {
		exitError(fmt.Errorf("failed to create GPTScript client: %w", err))
	}

	for _, u := range strings.Split(os.Getenv("TELEGRAM_BOT_ALLOWED_USERIDS"), ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		uid, err := strconv.ParseInt(u, 10, 64)
		if err != nil {
			exitError(fmt.Errorf("invalid allowed user id: %v", u))
		}
		allowedUserIDs[uid] = struct{}{}
	}

	for _, u := range strings.Split(os.Getenv("TELEGRAM_BOT_ALLOWED_USERNAMES"), ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		allowedUserNames[u] = struct{}{}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("http://127.0.0.1:" + os.Getenv("PORT")))
	})
	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		// read Message from the queue
		for {
			select {
			case msg := <-messageQueue:
				slog.Info("Found Message", "Message", msg)
				if msg.VoiceURL != "" {
					voiceID, err := uploadToWs(r.Context(), gClient, msg.VoiceURL, msg.FileExt)
					if err != nil {
						slog.Error("Failed to upload voice file", "error", err)
						http.Error(w, "Failed to upload voice file", http.StatusInternalServerError)
						return
					}
					msg.Text = fmt.Sprintf("<INFO>This message contains a voice file which you can find in the workspace at %s<INFO>\n<MESSAGE>%s</MESSAGE>", voiceID, msg)
				} else if msg.ImageURL != "" {
					imageID, err := uploadToWs(r.Context(), gClient, msg.ImageURL, msg.FileExt)
					if err != nil {
						slog.Error("Failed to upload image file", "error", err)
						http.Error(w, "Failed to upload image file", http.StatusInternalServerError)
						return
					}
					msg.Text = fmt.Sprintf("<INFO>This message contains an image file which you can find in the workspace at %s<INFO>\n<MESSAGE>%s</MESSAGE>", imageID, msg)
				}

				msgBytes, err := json.Marshal(msg)
				if err != nil {
					slog.Error("Failed to marshal message", "error", err)
					http.Error(w, "Failed to marshal message", http.StatusInternalServerError)
					return
				}

				_, _ = w.Write(msgBytes)
			default:
				_, _ = w.Write([]byte("No messages\n"))
				return
			}
		}
	})

	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		// parse the request
		var req Message
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("Failed to read request body", "error", err)
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			slog.Error("Failed to parse request", "error", err, "body", string(bodyBytes))
			http.Error(w, "Failed to parse request", http.StatusBadRequest)
			return
		}

		chatID, err := strconv.ParseInt(req.ChatID, 10, 64)
		if err != nil {
			slog.Error("Invalid chat ID", "ChatID", req.ChatID)
			http.Error(w, "Invalid chat ID", http.StatusBadRequest)
			return
		}

		// send the Message
		msg := telegram.NewMessage(chatID, req.Text)

		if req.MsgID != "" {
			msgID, err := strconv.Atoi(req.MsgID)
			if err != nil {
				slog.Error("Invalid Message ID", "MsgID", req.MsgID)
				http.Error(w, "Invalid Message ID", http.StatusBadRequest)
				return
			}
			msg.ReplyToMessageID = msgID
		}
		_, err = bot.Send(msg)
		if err != nil {
			http.Error(w, "Failed to send Message", http.StatusInternalServerError)
			return
		}
	})

	httpServer := &http.Server{
		Addr:    "127.0.0.1:" + os.Getenv("PORT"),
		Handler: mux,
	}

	bot.Debug = true

	slog.Info("Authorized on account", "account", bot.Self.UserName)

	u := telegram.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	slog.Info("Telegram bot started")
	go func() {
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			exitError(err)
		}
	}()

	for update := range updates {
		if update.Message == nil {
			continue
		}

		user := update.SentFrom()
		if !isAuthorized(user) {
			slog.Warn("Unauthorized user", "userID", user.ID, "userName", user.UserName)
			continue
		}

		slog.Info("Received a Message", "Message", update.Message.Text, "from", user)
		m := Message{
			ChatID: fmt.Sprintf("%d", update.Message.Chat.ID),
			MsgID:  fmt.Sprintf("%d", update.Message.MessageID),
			Text:   update.Message.Text,
			User:   fmt.Sprintf("%s %s (%s)", user.FirstName, user.LastName, user.UserName),
		}

		if update.Message.Voice != nil {
			slog.Info("Received a Voice Message", "Voice", update.Message.Voice)
			f, err := bot.GetFile(telegram.FileConfig{FileID: update.Message.Voice.FileID})
			if err != nil {
				slog.Error("Failed to get Voice URL", "error", err)
				continue
			}
			m.VoiceURL = f.Link(bot.Token)
			m.FileExt = filepath.Ext(f.FilePath)

		}

		if update.Message.Photo != nil {
			slog.Info("Received a Photo Message", "Photo", update.Message.Photo)
			f, err := bot.GetFile(telegram.FileConfig{FileID: update.Message.Photo[0].FileID})
			if err != nil {
				slog.Error("Failed to get Photo URL", "error", err)
				continue
			}
			m.ImageURL = f.Link(bot.Token)
			m.FileExt = filepath.Ext(f.FilePath)

		}

		messageQueue <- m

	}
}
