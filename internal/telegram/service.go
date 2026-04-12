package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"mantisx/internal/mcp"
	"mantisx/internal/memory"
)

type Service struct {
	MCP    mcp.Controller
	Memory *memory.LayerStore
}

var rememberPending = struct {
	mu sync.Mutex
	m  map[string]struct{}
}{
	m: map[string]struct{}{},
}

const (
	msgEmpty             = "\ube48 \uba54\uc2dc\uc9c0\ub294 \ucc98\ub9ac\ud560 \uc218 \uc5c6\uc2b5\ub2c8\ub2e4."
	msgStart             = "🪲 MantisX\n\nReady.\n\n그냥 말하면 됩니다.\n필요하면 /help\n\n(승인 요청은 1~4로 빠르게 처리 가능)"
	msgHelp              = "🦗 MantisX\n\n사용 가능한 명령:\n/help, /start\n/clear, /stop_mcp\n/approvals, /approve [id], /deny [id]\n/approval_mode\n/approval_on, /approval on\n/approval_off, /approval off\n1, 2, 3, 4 (빠른 승인)\n/memory, /remember <text>, /remember\n/remember_cancel, /memory_clear\n/server_tools"
	msgClearOK           = "MCP \uc138\uc158\uc744 \uc0c8\ub85c \uc2dc\uc791\ud588\uc2b5\ub2c8\ub2e4."
	msgStopOK            = "MCP \ub370\ubaac\uc744 \uc885\ub8cc\ud588\uc2b5\ub2c8\ub2e4."
	msgClearUnsupported  = "\uc5f0\uacb0\ub41c daemon \ubc84\uc804\uc774 /clear(\uc138\uc158 \ub9ac\uc14b)\ub97c \uc9c0\uc6d0\ud558\uc9c0 \uc54a\uc2b5\ub2c8\ub2e4. daemon\uc744 \ucd5c\uc2e0\uc73c\ub85c \uad50\uccb4\ud55c \ub4a4 \ub2e4\uc2dc \uc2dc\ub3c4\ud558\uc138\uc694."
	msgStopUnsupported   = "\uc5f0\uacb0\ub41c daemon \ubc84\uc804\uc774 /stop_mcp\ub97c \uc9c0\uc6d0\ud558\uc9c0 \uc54a\uc2b5\ub2c8\ub2e4."
	msgSubmitUnsupported = "\uc5f0\uacb0\ub41c daemon \ubc84\uc804\uc774 \uc77c\ubc18 \ub300\ud654 Submit API\ub97c \uc9c0\uc6d0\ud558\uc9c0 \uc54a\uc2b5\ub2c8\ub2e4. daemon \ubc84\uc804 \ud655\uc778\uc774 \ud544\uc694\ud569\ub2c8\ub2e4."
)

func (s Service) HandleText(ctx context.Context, text string) (string, error) {
	t := strings.TrimSpace(text)
	approvalLike := isApprovalIntentText(t)
	switch {
	case t == "":
		return msgEmpty, nil
	case t == "/start":
		return msgStart, nil
	case t == "/help":
		return msgHelp, nil
	case t == "/clear":
		if err := s.MCP.ClearSession(ctx); err != nil {
			if mcp.IsMethodUnavailable(err) {
				return msgClearUnsupported, nil
			}
			return "MCP \uc138\uc158 \ub9ac\uc14b \uc2e4\ud328: " + err.Error(), err
		}
		return msgClearOK, nil
	case t == "/memory":
		return s.renderMemory(ctx), nil
	case t == "/server_tools":
		return renderServerToolsStatus(), nil
	case t == "/remember":
		if s.Memory == nil {
			return "memory store is disabled.", nil
		}
		setRememberPending(mcp.SessionKeyFromContext(ctx), true)
		return "기억할 내용을 다음 메시지로 보내세요. 취소는 /remember_cancel", nil
	case strings.HasPrefix(t, "/remember "):
		if s.Memory == nil {
			return "memory store is disabled.", nil
		}
		payload := strings.TrimSpace(strings.TrimPrefix(t, "/remember"))
		if payload == "" {
			return "기억할 내용을 입력하세요. 예: /remember 이 프로젝트 루트는 c:/mantisx", nil
		}
		if err := s.Memory.RememberLong(mcp.SessionKeyFromContext(ctx), payload); err != nil {
			return "장기기억 저장 실패: " + err.Error(), err
		}
		setRememberPending(mcp.SessionKeyFromContext(ctx), false)
		return "장기기억에 저장했습니다.", nil
	case t == "/remember_cancel":
		setRememberPending(mcp.SessionKeyFromContext(ctx), false)
		return "장기기억 입력 대기를 취소했습니다.", nil
	case t == "/memory_clear":
		if s.Memory == nil {
			return "memory store is disabled.", nil
		}
		if err := s.Memory.ClearSession(mcp.SessionKeyFromContext(ctx)); err != nil {
			return "기억 삭제 실패: " + err.Error(), err
		}
		return "현재 세션의 3층 기억을 삭제했습니다.", nil
	case t == "/stop_mcp":
		if err := s.MCP.StopDaemon(ctx); err != nil {
			if mcp.IsMethodUnavailable(err) {
				return msgStopUnsupported, nil
			}
			return "MCP \uc885\ub8cc \uc2e4\ud328: " + err.Error(), err
		}
		return msgStopOK, nil
	case t == "/rebuild_restart":
		if err := launchRebuildRestartScript(); err != nil {
			return "재기동 스크립트 실행 실패: " + err.Error(), err
		}
		return "재기동 요청을 접수했습니다. 빌드 성공 시 서버를 순차 재시작합니다.", nil
	case t == "/approvals":
		list, err := s.MCP.ListApprovals(ctx)
		if err != nil {
			return "\uc2b9\uc778 \ubaa9\ub85d \uc870\ud68c \uc2e4\ud328: " + err.Error(), err
		}
		if len(list) == 0 {
			return "\ub300\uae30 \uc911\uc778 \uc2b9\uc778 \uc694\uccad\uc774 \uc5c6\uc2b5\ub2c8\ub2e4.", nil
		}
		var b strings.Builder
		b.WriteString("\ub300\uae30 \uc911 \uc2b9\uc778 \uc694\uccad\n")
		for _, it := range list {
			b.WriteString("- ")
			b.WriteString(it.ID)
			if strings.TrimSpace(string(it.Kind)) != "" {
				b.WriteString(" [")
				b.WriteString(string(it.Kind))
				b.WriteString("]")
			}
			b.WriteString(": ")
			b.WriteString(it.Summary)
			if strings.TrimSpace(it.Command) != "" {
				b.WriteString("\n  command: ")
				b.WriteString(it.Command)
			}
			if len(it.Files) > 0 {
				b.WriteString("\n  files: ")
				b.WriteString(strings.Join(it.Files, ", "))
			}
			b.WriteString("\n")
		}
		b.WriteString("승인: /approve [id]\n거부: /deny [id]\n빠른 입력: 1=허용, 2=세션 허용, 3=거절, 4=취소")
		return b.String(), nil
	case t == "/approval_mode":
		if s.MCP.ApprovalEnabled(ctx) {
			return "approval mode: ON", nil
		}
		return "approval mode: OFF", nil
	case t == "/approval_on" || t == "/approval on":
		return s.MCP.SetApprovalEnabled(ctx, true)
	case t == "/approval_off" || t == "/approval off":
		return s.MCP.SetApprovalEnabled(ctx, false)
	case strings.HasPrefix(t, "/approve"):
		parts := strings.Fields(t)
		id := ""
		if len(parts) >= 2 {
			id = parts[1]
		}
		return s.resolveApprovalCommand(ctx, id, true)
	case strings.HasPrefix(t, "/deny"):
		parts := strings.Fields(t)
		id := ""
		if len(parts) >= 2 {
			id = parts[1]
		}
		return s.resolveApprovalCommand(ctx, id, false)
	case t == "1" || t == "2" || t == "3" || t == "4":
		return s.resolveApprovalByChoice(ctx, t)
	case approvalLike:
		list, err := s.MCP.ListApprovals(ctx)
		if err != nil {
			return "승인 목록 조회 실패: " + err.Error(), err
		}
		if len(list) == 0 {
			return "현재 대기 중인 승인 요청이 없습니다. 승인 버튼이 뜨거나 `/approvals`에 항목이 있을 때만 승인 입력을 사용하세요.", nil
		}
		var b strings.Builder
		b.WriteString("승인 대기 요청이 있습니다.\n")
		for _, it := range list {
			b.WriteString("- ")
			b.WriteString(it.ID)
			b.WriteString(": ")
			b.WriteString(it.Summary)
			b.WriteString("\n")
		}
		b.WriteString("빠른 승인: 1=허용, 2=세션 허용, 3=거절, 4=취소")
		return strings.TrimSpace(b.String()), nil
	default:
		if s.Memory != nil && isRememberPending(mcp.SessionKeyFromContext(ctx)) {
			if strings.HasPrefix(t, "/") {
				return "장기기억 입력 대기 중입니다. 내용을 보내거나 /remember_cancel로 취소하세요.", nil
			}
			if err := s.Memory.RememberLong(mcp.SessionKeyFromContext(ctx), t); err != nil {
				return "장기기억 저장 실패: " + err.Error(), err
			}
			setRememberPending(mcp.SessionKeyFromContext(ctx), false)
			return "장기기억에 저장했습니다.", nil
		}
		prompt := t
		if s.Memory != nil {
			prompt = s.Memory.BuildPrompt(t, mcp.SessionKeyFromContext(ctx))
		}
		out, err := s.MCP.SubmitPrompt(ctx, prompt)
		if err != nil {
			if mcp.IsMethodUnavailable(err) {
				return msgSubmitUnsupported, nil
			}
			if reqErr, ok := mcp.AsApprovalRequired(err); ok {
				msg := "권한 승인 요청이 도착했습니다.\nID: " + reqErr.Request.ID
				if strings.TrimSpace(string(reqErr.Request.Kind)) != "" {
					msg += "\n종류: " + string(reqErr.Request.Kind)
				}
				msg += "\n요청: " + reqErr.Request.Summary
				if strings.TrimSpace(reqErr.Request.Command) != "" {
					msg += "\n명령: " + reqErr.Request.Command
				}
				if len(reqErr.Request.Files) > 0 {
					msg += "\n파일: " + strings.Join(reqErr.Request.Files, ", ")
				}
				msg += "\n승인: /approve " + reqErr.Request.ID + "\n거부: /deny " + reqErr.Request.ID + "\n빠른 입력: 1=허용, 2=세션 허용, 3=거절, 4=취소"
				return msg, nil
			}
			low := strings.ToLower(err.Error())
			if strings.Contains(low, "mcp initialize: context deadline exceeded") {
				return "Codex daemon initialize 응답이 시간 내 도착하지 않았습니다. daemon이 멈췄거나 과부하일 수 있습니다. daemon 재시작 후 다시 시도하세요.", err
			}
			if strings.Contains(low, "rg:") && strings.Contains(low, "os error 123") {
				return "Codex 요청 실패: rg 경로 글롭 형식 오류(os error 123). Windows에서는 파일 경로에 '*'를 직접 넣지 말고 `rg --glob` 형태로 다시 시도하세요.", err
			}
			return "Codex \uc694\uccad \uc2e4\ud328: " + err.Error(), err
		}
		if s.Memory != nil {
			_ = s.Memory.RecordTurn(mcp.SessionKeyFromContext(ctx), t, out)
		}
		return out, nil
	}
}

func (s Service) renderMemory(ctx context.Context) string {
	if s.Memory == nil {
		return "memory store is disabled."
	}
	snap := s.Memory.Snapshot(mcp.SessionKeyFromContext(ctx))
	var b strings.Builder
	b.WriteString("3층 기억 상태\n")
	b.WriteString(fmt.Sprintf("- 장기기억: %d\n", len(snap.Long)))
	b.WriteString(fmt.Sprintf("- 중기기억: %d\n", len(snap.Mid)))
	b.WriteString(fmt.Sprintf("- 임시기억: %d\n", len(snap.Temp)))
	if len(snap.Long) > 0 {
		b.WriteString("\n[장기기억]\n")
		for _, it := range tailMemory(snap.Long, 5) {
			b.WriteString("- ")
			b.WriteString(it)
			b.WriteString("\n")
		}
	}
	if len(snap.Mid) > 0 {
		b.WriteString("\n[중기기억]\n")
		for _, it := range tailMemory(snap.Mid, 4) {
			b.WriteString("- ")
			b.WriteString(it)
			b.WriteString("\n")
		}
	}
	if len(snap.Temp) > 0 {
		b.WriteString("\n[임시기억]\n")
		for _, it := range tailMemory(snap.Temp, 3) {
			b.WriteString("- ")
			b.WriteString(it)
			b.WriteString("\n")
		}
	}
	if strings.TrimSpace(snap.UpdatedAt) != "" {
		b.WriteString("\n업데이트: ")
		b.WriteString(snap.UpdatedAt)
	}
	return strings.TrimSpace(b.String())
}

func tailMemory(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func (s Service) resolveApprovalByChoice(ctx context.Context, choice string) (string, error) {
	list, err := s.MCP.ListApprovals(ctx)
	if err != nil {
		return "승인 목록 조회 실패: " + err.Error(), err
	}
	if len(list) == 0 {
		return "대기 중인 승인 요청이 없습니다.", nil
	}
	if len(list) > 1 {
		return "대기 승인 요청이 여러 개입니다. 숫자 입력 대신 `/approve <id>` 또는 `/deny <id>`를 사용하세요. (`/approvals`로 목록 확인)", nil
	}
	id := list[0].ID
	switch choice {
	case "1":
		out, err := resolveApprovalDecision(s.MCP, ctx, id, "accept")
		if err != nil {
			return "승인 처리 실패: " + err.Error(), err
		}
		return out, nil
	case "2":
		out, err := resolveApprovalDecision(s.MCP, ctx, id, "acceptForSession")
		if err != nil {
			return "승인 처리 실패: " + err.Error(), err
		}
		return out, nil
	case "4":
		out, err := resolveApprovalDecision(s.MCP, ctx, id, "cancel")
		if err != nil {
			return "취소 처리 실패: " + err.Error(), err
		}
		return out, nil
	default:
		out, err := resolveApprovalDecision(s.MCP, ctx, id, "decline")
		if err != nil {
			return "거부 처리 실패: " + err.Error(), err
		}
		return out, nil
	}
}

func resolveApprovalDecision(ctrl mcp.Controller, ctx context.Context, id string, decision string) (string, error) {
	return ctrl.ResolveApprovalDecision(ctx, id, mcp.NormalizeApprovalDecision(decision))
}

func (s Service) resolveApprovalCommand(ctx context.Context, id string, approve bool) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		list, err := s.MCP.ListApprovals(ctx)
		if err != nil {
			return "승인 목록 조회 실패: " + err.Error(), err
		}
		if len(list) == 0 {
			return "대기 중인 승인 요청이 없습니다.", nil
		}
		if len(list) > 1 {
			if approve {
				return "여러 승인 요청이 대기 중입니다. `/approve <id>` 형식으로 ID를 지정하세요. (`/approvals`로 목록 확인)", nil
			}
			return "여러 승인 요청이 대기 중입니다. `/deny <id>` 형식으로 ID를 지정하세요. (`/approvals`로 목록 확인)", nil
		}
		id = list[0].ID
	}
	out, err := mcp.ResolveApprovalBool(s.MCP, ctx, id, approve)
	if err != nil {
		if approve {
			return "승인 처리 실패: " + err.Error(), err
		}
		return "거부 처리 실패: " + err.Error(), err
	}
	return out, nil
}

func isApprovalIntentText(t string) bool {
	v := strings.ToLower(strings.TrimSpace(t))
	if v == "" {
		return false
	}
	switch v {
	case "승인", "허용", "거절", "approve", "deny":
		return true
	default:
		return false
	}
}

func setRememberPending(sessionKey string, pending bool) {
	key := normalizeMemorySessionKey(sessionKey)
	rememberPending.mu.Lock()
	defer rememberPending.mu.Unlock()
	if pending {
		rememberPending.m[key] = struct{}{}
		return
	}
	delete(rememberPending.m, key)
}

func isRememberPending(sessionKey string) bool {
	key := normalizeMemorySessionKey(sessionKey)
	rememberPending.mu.Lock()
	defer rememberPending.mu.Unlock()
	_, ok := rememberPending.m[key]
	return ok
}

func launchRebuildRestartScript() error {
	root := strings.TrimSpace(os.Getenv("MANTISX_ROOT_DIR"))
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	if root == "" {
		root = `C:\mantisx`
	}
	trigger := filepath.Join(root, "trigger_rebuild_restart_mantisx_stack.bat")
	if _, err := os.Stat(trigger); err != nil {
		trigger = filepath.Join(root, "rebuild_restart_mantisx_stack.bat")
	}
	// Must run detached from current server process to avoid self-kill before reply delivery.
	cmd := exec.Command("cmd", "/c", "start", "", "/min", trigger)
	cmd.Dir = root
	return cmd.Run()
}

func normalizeMemorySessionKey(sessionKey string) string {
	key := strings.TrimSpace(sessionKey)
	if key == "" {
		return "default"
	}
	return key
}

func renderServerToolsStatus() string {
	root := resolveRootDir()
	backendMode := strings.ToLower(strings.TrimSpace(os.Getenv("MANTISX_BACKEND_MODE")))
	if backendMode == "" {
		backendMode = "appserver(default)"
	}
	manifest, manifestSource := loadToolManifest(root)

	var b strings.Builder
	b.WriteString("서버 툴 점검\n")
	b.WriteString("- workspace: ")
	b.WriteString(root)
	b.WriteString("\n")
	b.WriteString("- backend_mode: ")
	b.WriteString(backendMode)
	b.WriteString("\n")
	b.WriteString("- platform: ")
	b.WriteString(runtime.GOOS)
	b.WriteString("/")
	b.WriteString(runtime.GOARCH)
	b.WriteString("\n")
	b.WriteString("- manifest: ")
	b.WriteString(manifestSource)
	b.WriteString("\n")

	for _, tc := range manifest.ToolChecks {
		appendToolStatus(&b, tc.Name, resolveToolValue(tc))
	}
	for _, fc := range manifest.FileChecks {
		appendFileStatus(&b, fc.Name, resolveManifestPath(root, fc.Path))
	}
	for _, mc := range manifest.MCPChecks {
		appendFileStatus(&b, "mcp/"+mc.Name, resolveManifestPath(root, mc.Path))
	}
	for _, sc := range manifest.SkillChecks {
		appendFileStatus(&b, "skill/"+sc.Name, resolveManifestPath(root, sc.Path))
	}

	appendFileStatus(&b, "tools/tools", filepath.Join(root, "tools", "tools"))
	appendFileStatus(&b, "tools/mcp", filepath.Join(root, "tools", "mcp"))
	appendFileStatus(&b, "tools/skills", filepath.Join(root, "tools", "skills"))

	daemonAddr := strings.TrimSpace(os.Getenv("MANTISX_CODEX_DAEMON_ADDR"))
	if daemonAddr == "" {
		daemonAddr = "127.0.0.1:17997(default)"
	}
	b.WriteString("- rpc_daemon_addr: ")
	b.WriteString(daemonAddr)
	b.WriteString(" (RPC 모드에서 사용)\n")
	b.WriteString("- 참고: codex_daemon_probe.exe는 RPC 레거시 모드에서만 필요")
	return b.String()
}

type toolManifest struct {
	ToolChecks  []manifestToolCheck `json:"tool_checks"`
	FileChecks  []manifestFileCheck `json:"file_checks"`
	MCPChecks   []manifestPathCheck `json:"mcp_checks"`
	SkillChecks []manifestPathCheck `json:"skill_checks"`
}

type manifestToolCheck struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Env     string `json:"env"`
	Default string `json:"default"`
}

type manifestFileCheck struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type manifestPathCheck struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func resolveRootDir() string {
	root := strings.TrimSpace(os.Getenv("MANTISX_ROOT_DIR"))
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	if root == "" {
		root = `C:\mantisx`
	}
	return root
}

func defaultToolManifest() toolManifest {
	return toolManifest{
		ToolChecks: []manifestToolCheck{
			{Name: "codex", Env: "MANTISX_CODEX_EXE", Default: "codex"},
			{Name: "go", Command: "go"},
			{Name: "cmd", Command: "cmd"},
			{Name: "powershell", Command: "powershell"},
			{Name: "taskkill", Command: "taskkill"},
			{Name: "netstat", Command: "netstat"},
		},
		FileChecks: []manifestFileCheck{
			{Name: "mantisx_server.exe", Path: filepath.Join("cmd", "bin", "mantisx_server.exe")},
			{Name: "run_mantisx_stack.bat", Path: "run_mantisx_stack.bat"},
			{Name: "rebuild_restart_mantisx_stack.bat", Path: "rebuild_restart_mantisx_stack.bat"},
			{Name: "trigger_rebuild_restart_mantisx_stack.bat", Path: "trigger_rebuild_restart_mantisx_stack.bat"},
		},
		MCPChecks: []manifestPathCheck{
			{Name: "registry", Path: filepath.Join("tools", "mcp", "registry.json")},
		},
		SkillChecks: []manifestPathCheck{
			{Name: "registry", Path: filepath.Join("tools", "skills", "registry.json")},
		},
	}
}

func loadToolManifest(root string) (toolManifest, string) {
	manifest := defaultToolManifest()
	path := strings.TrimSpace(os.Getenv("MANTISX_TOOLS_MANIFEST"))
	if path == "" {
		path = filepath.Join(root, "tools", "tools", "manifest.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest, "default (missing external manifest)"
	}
	var ext toolManifest
	if err := json.Unmarshal(data, &ext); err != nil {
		return manifest, "default (invalid external manifest)"
	}
	if len(ext.ToolChecks) > 0 {
		manifest.ToolChecks = ext.ToolChecks
	}
	if len(ext.FileChecks) > 0 {
		manifest.FileChecks = ext.FileChecks
	}
	if len(ext.MCPChecks) > 0 {
		manifest.MCPChecks = ext.MCPChecks
	}
	if len(ext.SkillChecks) > 0 {
		manifest.SkillChecks = ext.SkillChecks
	}
	return manifest, path
}

func resolveManifestPath(root, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}

func resolveToolValue(tc manifestToolCheck) string {
	if v := strings.TrimSpace(tc.Command); v != "" {
		return v
	}
	if envKey := strings.TrimSpace(tc.Env); envKey != "" {
		if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
			return v
		}
	}
	return strings.TrimSpace(tc.Default)
}

func appendToolStatus(b *strings.Builder, name, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		b.WriteString("- ")
		b.WriteString(name)
		b.WriteString(": missing (empty)\n")
		return
	}
	if filepath.IsAbs(value) || strings.Contains(value, `\`) || strings.Contains(value, "/") {
		if _, err := os.Stat(value); err == nil {
			b.WriteString("- ")
			b.WriteString(name)
			b.WriteString(": ok (")
			b.WriteString(value)
			b.WriteString(")\n")
			return
		}
		b.WriteString("- ")
		b.WriteString(name)
		b.WriteString(": missing (")
		b.WriteString(value)
		b.WriteString(")\n")
		return
	}
	if p, err := exec.LookPath(value); err == nil {
		b.WriteString("- ")
		b.WriteString(name)
		b.WriteString(": ok (")
		b.WriteString(p)
		b.WriteString(")\n")
		return
	}
	b.WriteString("- ")
	b.WriteString(name)
	b.WriteString(": missing (")
	b.WriteString(value)
	b.WriteString(")\n")
}

func appendFileStatus(b *strings.Builder, name, fullPath string) {
	if _, err := os.Stat(fullPath); err == nil {
		b.WriteString("- ")
		b.WriteString(name)
		b.WriteString(": ok\n")
		return
	}
	b.WriteString("- ")
	b.WriteString(name)
	b.WriteString(": missing\n")
}
