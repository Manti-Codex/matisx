package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mantisx/internal/settings"
	"mantisx/internal/telegram"
)

type bootState struct {
	LastStartedAt string `json:"last_started_at"`
}

type startupSummary struct {
	Now               time.Time
	IsRestart         bool
	PreviousStartedAt string
	MemoryPath        string
	MemoryStoreReady  bool
	MemorySessionCnt  int
}

func readAndUpdateBootState(path string, now time.Time) (startupSummary, error) {
	out := startupSummary{Now: now}
	b, err := os.ReadFile(path)
	if err == nil {
		var prev bootState
		if json.Unmarshal(b, &prev) == nil {
			prevAt := strings.TrimSpace(prev.LastStartedAt)
			if prevAt != "" {
				out.IsRestart = true
				out.PreviousStartedAt = prevAt
			}
		}
	} else if !os.IsNotExist(err) {
		return out, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return out, err
	}
	next := bootState{LastStartedAt: now.Format(time.RFC3339)}
	w, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return out, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, w, 0o600); err != nil {
		return out, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return out, err
	}
	return out, nil
}

func renderStartupNotice(sum startupSummary) string {
	startState := "첫 실행"
	if sum.IsRestart {
		startState = "재시작"
	}
	memState := "비활성/로드 실패"
	if sum.MemoryStoreReady {
		memState = fmt.Sprintf("로드됨 (sessions=%d)", sum.MemorySessionCnt)
	}
	var b strings.Builder
	b.WriteString("🦗 MantisX 부팅 알림\n")
	b.WriteString("- 상태: ")
	b.WriteString(startState)
	b.WriteString("\n")
	if sum.IsRestart && strings.TrimSpace(sum.PreviousStartedAt) != "" {
		b.WriteString("- 이전 시작: ")
		b.WriteString(sum.PreviousStartedAt)
		b.WriteString("\n")
	}
	b.WriteString("- 메모리: ")
	b.WriteString(memState)
	b.WriteString("\n")
	if strings.TrimSpace(sum.MemoryPath) != "" {
		b.WriteString("- 파일: ")
		b.WriteString(sum.MemoryPath)
		b.WriteString("\n")
	}
	b.WriteString("- 현재 시작: ")
	b.WriteString(sum.Now.Format(time.RFC3339))
	return b.String()
}

func notifyStartup(cfg settings.TelegramSettings, sum startupSummary) {
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return
	}
	allowed := telegram.ParseAllowedUsers(cfg.AllowedUsers)
	if len(allowed) == 0 {
		log.Printf("[mantisx] startup notify skipped: allowed_users is empty")
		return
	}
	msg := renderStartupNotice(sum)
	for _, chatID := range allowed {
		if err := telegram.SendTextMessage(token, chatID, msg); err != nil {
			log.Printf("[mantisx] startup notify failed (chat_id=%d): %v", chatID, err)
		}
	}
}

