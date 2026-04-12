package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type TelegramSettings struct {
	Token           string `json:"token"`
	AllowedUsers    string `json:"allowed_users"`
	WorkspaceDir    string `json:"workspace_dir"`
	CodexDaemonAddr string `json:"codex_daemon_addr"`
	UpdatedAt       string `json:"updated_at"`
}

type Store struct {
	path string
	mu   sync.RWMutex
}

var telegramTokenRe = regexp.MustCompile(`^[0-9]{7,12}:[A-Za-z0-9_-]{20,}$`)

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Load() (TelegramSettings, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out TelegramSettings
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			out.CodexDaemonAddr = defaultDaemonAddr()
			return out, nil
		}
		return out, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		out.CodexDaemonAddr = defaultDaemonAddr()
		return out, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return TelegramSettings{}, err
	}
	if strings.TrimSpace(out.CodexDaemonAddr) == "" {
		out.CodexDaemonAddr = defaultDaemonAddr()
	}
	return out, nil
}

func (s *Store) Save(in TelegramSettings) (TelegramSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	in.Token = strings.TrimSpace(in.Token)
	in.AllowedUsers = strings.TrimSpace(in.AllowedUsers)
	in.WorkspaceDir = strings.TrimSpace(in.WorkspaceDir)
	in.CodexDaemonAddr = strings.TrimSpace(in.CodexDaemonAddr)
	if in.CodexDaemonAddr == "" {
		in.CodexDaemonAddr = defaultDaemonAddr()
	}
	if err := validate(in); err != nil {
		return TelegramSettings{}, err
	}
	in.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return TelegramSettings{}, err
	}
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return TelegramSettings{}, err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return TelegramSettings{}, err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return TelegramSettings{}, err
	}
	return in, nil
}

func defaultDaemonAddr() string {
	if v := strings.TrimSpace(os.Getenv("MANTISX_CODEX_DAEMON_ADDR")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("BACKTESTER_CODEX_DAEMON_ADDR")); v != "" {
		return v
	}
	return "127.0.0.1:17997"
}

func validate(in TelegramSettings) error {
	if len(in.Token) > 512 {
		return errors.New("token too long")
	}
	if in.Token != "" && !telegramTokenRe.MatchString(in.Token) {
		return errors.New("invalid telegram token format (expected: <bot_id>:<token>)")
	}
	if len(in.AllowedUsers) > 2048 {
		return errors.New("allowed_users too long")
	}
	if len(in.WorkspaceDir) > 1024 {
		return errors.New("workspace_dir too long")
	}
	if len(in.CodexDaemonAddr) > 128 {
		return errors.New("codex_daemon_addr too long")
	}
	return nil
}
