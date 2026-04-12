package telegram

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mantisx/internal/mcp"
	"mantisx/internal/memory"
)

type mockMCP struct {
	clearCalled bool
	stopCalled  bool
	submitIn    string
	submitOut   string
	err         error
	approvalOn  bool
	approvals   []mcp.ApprovalRequest
	lastID      string
	lastApprove bool
}

func (m *mockMCP) ClearSession(ctx context.Context) error {
	m.clearCalled = true
	return m.err
}

func (m *mockMCP) StopDaemon(ctx context.Context) error {
	m.stopCalled = true
	return m.err
}

func (m *mockMCP) SubmitPrompt(ctx context.Context, prompt string) (string, error) {
	m.submitIn = prompt
	if m.err != nil {
		return "", m.err
	}
	return m.submitOut, nil
}

func (m *mockMCP) ListApprovals(ctx context.Context) ([]mcp.ApprovalRequest, error) {
	return m.approvals, nil
}

func (m *mockMCP) ResolveApprovalDecision(ctx context.Context, id string, decision mcp.ApprovalDecision) (string, error) {
	m.lastID = id
	m.lastApprove = decision == mcp.ApprovalDecisionAccept || decision == mcp.ApprovalDecisionAcceptForSession
	if m.err != nil {
		return "", m.err
	}
	if m.lastApprove {
		return "approved", nil
	}
	return "denied", nil
}
func (m *mockMCP) ApprovalEnabled(ctx context.Context) bool { return m.approvalOn }
func (m *mockMCP) SetApprovalEnabled(ctx context.Context, enabled bool) (string, error) {
	m.approvalOn = enabled
	if enabled {
		return "approval mode enabled", nil
	}
	return "approval mode disabled", nil
}

func TestHandleTextClear(t *testing.T) {
	m := &mockMCP{}
	s := Service{MCP: m}

	msg, err := s.HandleText(context.Background(), "/clear")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !m.clearCalled {
		t.Fatalf("clear was not called")
	}
	if msg == "" {
		t.Fatalf("expected message")
	}
}

func TestHandleTextSubmit(t *testing.T) {
	m := &mockMCP{submitOut: "ok-result"}
	s := Service{MCP: m}

	msg, err := s.HandleText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if m.submitIn != "hello" {
		t.Fatalf("unexpected submit input: %q", m.submitIn)
	}
	if msg != "ok-result" {
		t.Fatalf("unexpected message: %q", msg)
	}
}

func TestHandleTextSubmitError(t *testing.T) {
	m := &mockMCP{err: errors.New("rpc down")}
	s := Service{MCP: m}

	_, err := s.HandleText(context.Background(), "hello")
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestHandleTextServerTools(t *testing.T) {
	m := &mockMCP{}
	s := Service{MCP: m}

	msg, err := s.HandleText(context.Background(), "/server_tools")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(msg, "서버 툴 점검") {
		t.Fatalf("unexpected response: %q", msg)
	}
	if !strings.Contains(msg, "codex") {
		t.Fatalf("expected codex status in response: %q", msg)
	}
}

func TestRenderServerToolsStatusUsesExternalManifest(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tools", "tools"), 0o755); err != nil {
		t.Fatalf("mkdir tools/tools: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tools", "mcp"), 0o755); err != nil {
		t.Fatalf("mkdir tools/mcp: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tools", "skills"), 0o755); err != nil {
		t.Fatalf("mkdir tools/skills: %v", err)
	}
	manifest := `{
  "tool_checks": [
    {"name":"custom-tool","command":"cmd"}
  ],
  "file_checks": [
    {"name":"custom-script","path":"tools/tools/custom.bat"}
  ],
  "mcp_checks": [
    {"name":"sample","path":"tools/mcp/registry.json"}
  ],
  "skill_checks": [
    {"name":"sample","path":"tools/skills/registry.json"}
  ]
}`
	if err := os.WriteFile(filepath.Join(root, "tools", "tools", "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "tools", "tools", "custom.bat"), []byte("@echo off\r\necho ok\r\n"), 0o644); err != nil {
		t.Fatalf("write custom.bat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "tools", "mcp", "registry.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write mcp registry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "tools", "skills", "registry.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write skill registry: %v", err)
	}

	t.Setenv("MANTISX_ROOT_DIR", root)
	t.Setenv("MANTISX_TOOLS_MANIFEST", filepath.Join(root, "tools", "tools", "manifest.json"))

	msg := renderServerToolsStatus()
	if !strings.Contains(msg, "custom-tool") {
		t.Fatalf("expected custom-tool in status: %q", msg)
	}
	if !strings.Contains(msg, "custom-script: ok") {
		t.Fatalf("expected custom-script status: %q", msg)
	}
	if !strings.Contains(msg, "mcp/sample: ok") {
		t.Fatalf("expected mcp sample status: %q", msg)
	}
	if !strings.Contains(msg, "skill/sample: ok") {
		t.Fatalf("expected skill sample status: %q", msg)
	}
}

func TestHandleTextHelpHidesDevOnlyCommands(t *testing.T) {
	m := &mockMCP{}
	s := Service{MCP: m}

	msg, err := s.HandleText(context.Background(), "/help")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(msg, "사용 가능한 명령") {
		t.Fatalf("expected command list header: %q", msg)
	}
	if !strings.Contains(msg, "/server_tools") {
		t.Fatalf("expected command list contents: %q", msg)
	}
	if strings.Contains(msg, "/rebuild_restart") {
		t.Fatalf("dev-only command should be hidden from help: %q", msg)
	}
}

func TestHandleTextStartIsSimple(t *testing.T) {
	m := &mockMCP{}
	s := Service{MCP: m}

	msg, err := s.HandleText(context.Background(), "/start")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(msg, "Ready.") {
		t.Fatalf("expected simple start message: %q", msg)
	}
	if strings.Contains(msg, "사용 가능한 명령") {
		t.Fatalf("start should not include full command list: %q", msg)
	}
}

func TestHandleTextNumericApprovalChoice(t *testing.T) {
	m := &mockMCP{
		approvals: []mcp.ApprovalRequest{{ID: "apr-0001", Summary: "need approval"}},
	}
	s := Service{MCP: m}

	msg, err := s.HandleText(context.Background(), "1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if m.lastID != "apr-0001" || !m.lastApprove {
		t.Fatalf("unexpected approval call: id=%q approve=%v", m.lastID, m.lastApprove)
	}
	if msg == "" {
		t.Fatalf("expected message")
	}
}

func TestRememberTwoStepFlow(t *testing.T) {
	mem, err := memory.NewLayerStore(filepath.Join(t.TempDir(), "memory_layers.json"))
	if err != nil {
		t.Fatalf("new memory store: %v", err)
	}
	m := &mockMCP{}
	s := Service{MCP: m, Memory: mem}
	ctx := mcp.WithSessionKey(context.Background(), "tg:chat:remember-1")

	msg, err := s.HandleText(ctx, "/remember")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(msg, "다음 메시지") {
		t.Fatalf("unexpected response: %q", msg)
	}

	msg, err = s.HandleText(ctx, "이 사용자는 요약을 선호함")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(msg, "저장") {
		t.Fatalf("unexpected save response: %q", msg)
	}

	snap := mem.Snapshot("tg:chat:remember-1")
	if len(snap.Long) != 1 {
		t.Fatalf("expected one long memory, got %d", len(snap.Long))
	}
	if !strings.Contains(snap.Long[0], "요약을 선호함") {
		t.Fatalf("unexpected long memory: %q", snap.Long[0])
	}
}

func TestRememberCancel(t *testing.T) {
	mem, err := memory.NewLayerStore(filepath.Join(t.TempDir(), "memory_layers.json"))
	if err != nil {
		t.Fatalf("new memory store: %v", err)
	}
	m := &mockMCP{submitOut: "ok-result"}
	s := Service{MCP: m, Memory: mem}
	ctx := mcp.WithSessionKey(context.Background(), "tg:chat:remember-2")

	if _, err := s.HandleText(ctx, "/remember"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	msg, err := s.HandleText(ctx, "/remember_cancel")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(msg, "취소") {
		t.Fatalf("unexpected cancel response: %q", msg)
	}

	msg, err = s.HandleText(ctx, "일반 질문")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if msg != "ok-result" {
		t.Fatalf("expected submit response, got %q", msg)
	}
	if m.submitIn == "" {
		t.Fatalf("expected prompt submitted to MCP")
	}
	snap := mem.Snapshot("tg:chat:remember-2")
	if len(snap.Long) != 0 {
		t.Fatalf("expected no long memory after cancel, got %d", len(snap.Long))
	}
}
