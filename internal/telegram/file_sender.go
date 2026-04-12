package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FirstAllowedUserID parses allowed_users and returns the first positive id.
func FirstAllowedUserID(allowedUsers string) (int64, error) {
	if strings.TrimSpace(allowedUsers) == "" {
		return 0, fmt.Errorf("allowed_users is empty")
	}
	fields := strings.FieldsFunc(allowedUsers, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid allowed_users entry %q", f)
		}
		if v > 0 {
			return v, nil
		}
	}
	return 0, fmt.Errorf("no positive numeric id in allowed_users")
}

type sendDocResp struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
}

// SendTextMessage sends a plain text message to a Telegram chat.
func SendTextMessage(token string, chatID int64, text string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("empty token")
	}
	if chatID <= 0 {
		return fmt.Errorf("invalid chat id: %d", chatID)
	}
	msg := strings.TrimSpace(text)
	if msg == "" {
		return fmt.Errorf("empty text")
	}
	api := "https://api.telegram.org/bot" + token
	client := &http.Client{Timeout: 12 * time.Second}
	_, err := sendMessage(client, api, chatID, msg, nil)
	return err
}

// SendDocumentFile uploads a local file to Telegram chat as document.
func SendDocumentFile(token string, chatID int64, filePath, caption string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("empty token")
	}
	if chatID <= 0 {
		return fmt.Errorf("invalid chat id: %d", chatID)
	}
	if strings.TrimSpace(filePath) == "" {
		return fmt.Errorf("empty file path")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	if err := w.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	if strings.TrimSpace(caption) != "" {
		if err := w.WriteField("caption", caption); err != nil {
			return err
		}
	}

	part, err := w.CreateFormFile("document", filepath.Base(filePath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	url := "https://api.telegram.org/bot" + token + "/sendDocument"
	req, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out sendDocResp
	if err := json.Unmarshal(respBody, &out); err != nil {
		return fmt.Errorf("decode telegram response: %w (body=%s)", err, strings.TrimSpace(string(respBody)))
	}
	if !out.OK {
		return fmt.Errorf("telegram api error code=%d desc=%s", out.ErrorCode, out.Description)
	}
	return nil
}
