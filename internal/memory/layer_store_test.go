package memory

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLayerStorePersistAndLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memory_layers.json")
	s, err := NewLayerStore(p)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	if err := s.RememberLong("tg:chat:1", "project root is c:/mantisx"); err != nil {
		t.Fatalf("remember long: %v", err)
	}
	if err := s.RecordTurn("tg:chat:1", "status 확인", "정상 응답"); err != nil {
		t.Fatalf("record turn: %v", err)
	}

	s2, err := NewLayerStore(p)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	got := s2.Snapshot("tg:chat:1")
	if len(got.Long) == 0 {
		t.Fatalf("expected long memory")
	}
	if len(got.Mid) == 0 {
		t.Fatalf("expected mid memory")
	}
	if len(got.Temp) == 0 {
		t.Fatalf("expected temp memory")
	}
}

func TestBuildPromptIncludesLayers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memory_layers.json")
	s, err := NewLayerStore(p)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	_ = s.RememberLong("tg:chat:2", "사용자 선호: 짧고 명확한 답변")
	_ = s.RecordTurn("tg:chat:2", "재시작해도 이어가", "메모리로 이어가겠습니다")

	prompt := s.BuildPrompt("지금 상태 알려줘", "tg:chat:2")
	if prompt == "지금 상태 알려줘" {
		t.Fatalf("expected prompt to include memory context")
	}
	if want := "장기기억"; !strings.Contains(prompt, want) {
		t.Fatalf("prompt missing %q", want)
	}
	if want := "사용자 현재 요청"; !strings.Contains(prompt, want) {
		t.Fatalf("prompt missing %q", want)
	}
}
