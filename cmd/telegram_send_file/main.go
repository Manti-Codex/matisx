package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"mantisx/internal/settings"
	"mantisx/internal/telegram"
)

func main() {
	settingsPath := flag.String("settings", "state/telegram_settings.json", "path to telegram settings json")
	filePath := flag.String("file", "", "path to file to send")
	caption := flag.String("caption", "", "telegram caption")
	chatIDFlag := flag.Int64("chat-id", 0, "target chat id; if 0 uses first allowed_users entry")
	flag.Parse()

	if strings.TrimSpace(*filePath) == "" {
		exitf("missing required -file")
	}

	cfg, err := settings.NewStore(*settingsPath).Load()
	if err != nil {
		exitf("read settings: %v", err)
	}
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		exitf("empty token in settings: %s", *settingsPath)
	}

	chatID := *chatIDFlag
	if chatID == 0 {
		chatID, err = telegram.FirstAllowedUserID(cfg.AllowedUsers)
		if err != nil {
			exitf("resolve chat id: %v", err)
		}
	}

	if err := telegram.SendDocumentFile(token, chatID, *filePath, *caption); err != nil {
		exitf("send document: %v", err)
	}
	fmt.Printf("sent: %s -> chat_id=%d\n", *filePath, chatID)
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
