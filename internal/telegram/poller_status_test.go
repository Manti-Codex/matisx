package telegram

import (
	"strings"
	"testing"
)

func TestInferSystemStatus(t *testing.T) {
	tests := []struct {
		elapsed int
		soft    int
		hard    int
		want    string
	}{
		{elapsed: 5, soft: 120, hard: 135, want: "THINKING"},
		{elapsed: 70, soft: 120, hard: 135, want: "RECOVERING"},
		{elapsed: 112, soft: 120, hard: 135, want: "TIMEOUT_WARNING"},
		{elapsed: 122, soft: 120, hard: 135, want: "TIMEOUT_EXTENDED"},
	}
	for _, tt := range tests {
		got := inferSystemStatus(tt.elapsed, tt.soft, tt.hard)
		if got != tt.want {
			t.Fatalf("elapsed=%d soft=%d hard=%d: want=%s got=%s", tt.elapsed, tt.soft, tt.hard, tt.want, got)
		}
	}
}

func TestRotatingInputWords(t *testing.T) {
	task := "stdin 단어 몇개를 보이게 바꿔줘"

	got0 := rotatingInputWords(task, 0)
	got2 := rotatingInputWords(task, 2)
	got4 := rotatingInputWords(task, 4)

	if got0 == "" || got2 == "" || got4 == "" {
		t.Fatalf("expected non-empty rotating words: %q / %q / %q", got0, got2, got4)
	}
	if got0 == got2 && got2 == got4 {
		t.Fatalf("expected rotation by elapsed time, got same outputs: %q", got0)
	}
}

func TestRenderProgressLineThinkingIncludesInputWords(t *testing.T) {
	got := renderProgressLine("THINKING", "thinking", "stdin 단어 몇개를 보이게 바꿔줘", 2)
	if got == "" {
		t.Fatalf("expected progress line")
	}
	if !containsAll(got, []string{"처리 중...", "분석 중 - "}) {
		t.Fatalf("expected thinking line with rotating words, got: %q", got)
	}
}

func containsAll(s string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
