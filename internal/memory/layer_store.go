package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxLongEntries = 24
	maxMidEntries  = 36
	maxTempEntries = 10
)

type SessionLayers struct {
	Long      []string `json:"long"`
	Mid       []string `json:"mid"`
	Temp      []string `json:"temp"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

type persistedState struct {
	Sessions map[string]SessionLayers `json:"sessions"`
}

type LayerStore struct {
	path  string
	mu    sync.Mutex
	state persistedState
}

func (s *LayerStore) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.state.Sessions)
}

func NewLayerStore(path string) (*LayerStore, error) {
	s := &LayerStore{
		path: path,
		state: persistedState{
			Sessions: map[string]SessionLayers{},
		},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *LayerStore) BuildPrompt(userInput string, sessionKey string) string {
	userInput = strings.TrimSpace(userInput)
	if userInput == "" {
		return ""
	}

	s.mu.Lock()
	mem := s.state.Sessions[normalizeSessionKey(sessionKey)]
	s.mu.Unlock()

	if len(mem.Long) == 0 && len(mem.Mid) == 0 && len(mem.Temp) == 0 {
		return userInput
	}

	var b strings.Builder
	b.WriteString("아래는 이전 대화에서 유지한 메모리입니다. 사실로 단정하지 말고 참고 맥락으로만 사용하세요.\n")

	appendList := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		b.WriteString("\n[")
		b.WriteString(title)
		b.WriteString("]\n")
		for i, it := range items {
			if i >= 12 {
				break
			}
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(it))
			b.WriteString("\n")
		}
	}
	appendList("장기기억", tail(mem.Long, 12))
	appendList("중기기억", tail(mem.Mid, 10))
	appendList("임시기억", tail(mem.Temp, 6))

	b.WriteString("\n[사용자 현재 요청]\n")
	b.WriteString(userInput)
	return b.String()
}

func (s *LayerStore) RecordTurn(sessionKey, userText, assistantText string) error {
	userText = clipText(userText, 300)
	assistantText = clipText(assistantText, 500)
	if strings.TrimSpace(userText) == "" && strings.TrimSpace(assistantText) == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := normalizeSessionKey(sessionKey)
	mem := s.state.Sessions[key]

	tempEntry := fmt.Sprintf("U: %s\nA: %s", oneLine(userText), oneLine(assistantText))
	mem.Temp = append(mem.Temp, tempEntry)
	mem.Temp = tail(mem.Temp, maxTempEntries)

	midEntry := buildMidSummary(userText, assistantText)
	if strings.TrimSpace(midEntry) != "" {
		mem.Mid = append(mem.Mid, midEntry)
		mem.Mid = tail(dedupe(mem.Mid), maxMidEntries)
	}

	mem.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	s.state.Sessions[key] = mem
	return s.saveLocked()
}

func (s *LayerStore) RememberLong(sessionKey, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	text = clipText(text, 240)

	s.mu.Lock()
	defer s.mu.Unlock()
	key := normalizeSessionKey(sessionKey)
	mem := s.state.Sessions[key]
	mem.Long = append(mem.Long, text)
	mem.Long = tail(dedupe(mem.Long), maxLongEntries)
	mem.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	s.state.Sessions[key] = mem
	return s.saveLocked()
}

func (s *LayerStore) Snapshot(sessionKey string) SessionLayers {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Sessions[normalizeSessionKey(sessionKey)]
}

func (s *LayerStore) ClearSession(sessionKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.state.Sessions, normalizeSessionKey(sessionKey))
	return s.saveLocked()
}

func (s *LayerStore) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	var st persistedState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	if st.Sessions == nil {
		st.Sessions = map[string]SessionLayers{}
	}
	s.state = st
	return nil
}

func (s *LayerStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func normalizeSessionKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return "default"
	}
	return k
}

func buildMidSummary(userText, assistantText string) string {
	u := oneLine(clipText(userText, 140))
	a := oneLine(clipText(assistantText, 170))
	if u == "" && a == "" {
		return ""
	}
	if u == "" {
		return "답변: " + a
	}
	if a == "" {
		return "요청: " + u
	}
	return "요청=" + u + " | 결과=" + a
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

func clipText(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "..."
}

func tail(in []string, n int) []string {
	if n <= 0 || len(in) == 0 {
		return nil
	}
	if len(in) <= n {
		out := make([]string, len(in))
		copy(out, in)
		return out
	}
	out := make([]string, n)
	copy(out, in[len(in)-n:])
	return out
}

func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, v := range in {
		key := strings.TrimSpace(v)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}
