package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type appPendingApproval struct {
	LocalID   string
	ServerID  interface{}
	Method    string
	TurnID    string
	ThreadID  string
	Requested ApprovalRequest
}

type appTurnResult struct {
	Content  string
	Err      error
	Approval *ApprovalRequest
}

type appTurnTracker struct {
	assistantBuf  strings.Builder
	commandBuf    strings.Builder
	finalText     string
	phase         string
	task          string
	taskUpdatedAt time.Time
	lastEventAt   time.Time
	completed     *appTurnResult
	waiter        chan appTurnResult
	approval      *ApprovalRequest
	approvalSent  bool
}

type appRPCResult struct {
	result map[string]interface{}
	err    error
}

type appSessionState struct {
	threadID    string
	approvalSeq int64
	turns       map[string]*appTurnTracker
	approvals   map[string]ApprovalRequest
	appByID     map[string]appPendingApproval
}

type AppServerController struct {
	CodexExe string
	CodexCwd string
	Args     []string

	workspaceDir string
	approvalSeq  int64

	approvalEnabled bool

	procMu      sync.Mutex
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	writeMu     sync.Mutex
	readerDone  chan struct{}
	started     bool
	initialized bool

	nextID int64

	stateMu         sync.Mutex
	pendingRPC      map[string]chan appRPCResult
	sessions        map[string]*appSessionState
	threadToSession map[string]string
}

const (
	appPhaseThinking      = "thinking"
	appPhaseToolRunning   = "tool_running"
	appPhaseApproval      = "approval_needed"
	appPhaseFinalAnswer   = "final_answer"
	appPhaseIgnore        = "ignore"
	assistantBufferMaxRun = 12000 // must match telegram.AssistantBufferMax
	commandBufferMaxRun   = 4000  // must match telegram.CommandBufferMax
)

func NewAppServerControllerFromEnv() *AppServerController {
	codexExe := strings.TrimSpace(os.Getenv("MANTISX_CODEX_EXE"))
	if codexExe == "" {
		codexExe = "codex"
	}
	codexCwd := strings.TrimSpace(os.Getenv("MANTISX_CODEX_CWD"))
	args := []string{"app-server", "--listen", "stdio://"}
	enabled := envBool("MANTISX_APPROVAL_ENABLED", true)
	return &AppServerController{
		CodexExe:        codexExe,
		CodexCwd:        codexCwd,
		Args:            args,
		approvalEnabled: enabled,
		nextID:          1000,
		pendingRPC:      map[string]chan appRPCResult{},
		sessions:        map[string]*appSessionState{},
		threadToSession: map[string]string{},
	}
}

func (c *AppServerController) SetWorkspaceDir(dir string) {
	c.procMu.Lock()
	c.workspaceDir = strings.TrimSpace(dir)
	c.procMu.Unlock()
}

func (c *AppServerController) sessionKey(ctx context.Context) string {
	return SessionKeyFromContext(ctx)
}

func (c *AppServerController) getSessionLocked(key string) *appSessionState {
	s := c.sessions[key]
	if s == nil {
		s = &appSessionState{
			turns:     map[string]*appTurnTracker{},
			approvals: map[string]ApprovalRequest{},
			appByID:   map[string]appPendingApproval{},
		}
		c.sessions[key] = s
	}
	return s
}

func (c *AppServerController) findSessionByThreadLocked(threadID string) *appSessionState {
	k := c.threadToSession[threadID]
	if k == "" {
		return nil
	}
	return c.sessions[k]
}

func (c *AppServerController) ApprovalEnabled(ctx context.Context) bool {
	c.procMu.Lock()
	defer c.procMu.Unlock()
	return c.approvalEnabled
}

func (c *AppServerController) SetApprovalEnabled(ctx context.Context, enabled bool) (string, error) {
	c.procMu.Lock()
	c.approvalEnabled = enabled
	c.procMu.Unlock()
	if enabled {
		return "approval mode enabled", nil
	}
	return "approval mode disabled", nil
}

func (c *AppServerController) ClearSession(ctx context.Context) error {
	sk := c.sessionKey(ctx)
	c.stateMu.Lock()
	s := c.getSessionLocked(sk)
	threadID := s.threadID
	if threadID != "" {
		delete(c.threadToSession, threadID)
	}
	s.threadID = ""
	s.turns = map[string]*appTurnTracker{}
	s.approvals = map[string]ApprovalRequest{}
	s.appByID = map[string]appPendingApproval{}
	c.stateMu.Unlock()

	if strings.TrimSpace(threadID) != "" {
		_, _ = c.request(ctx, "thread/archive", map[string]interface{}{"threadId": threadID})
	}
	return nil
}

func (c *AppServerController) StopDaemon(ctx context.Context) error {
	c.procMu.Lock()
	defer c.procMu.Unlock()
	c.shutdownLocked(errors.New("app-server stopped"))
	return nil
}

func (c *AppServerController) ListApprovals(ctx context.Context) ([]ApprovalRequest, error) {
	sk := c.sessionKey(ctx)
	c.stateMu.Lock()
	s := c.getSessionLocked(sk)
	defer c.stateMu.Unlock()
	out := make([]ApprovalRequest, 0, len(s.approvals))
	for _, v := range s.approvals {
		out = append(out, v)
	}
	// Preserve deterministic order.
	for i := 0; i < len(out)-1; i++ {
		for j := i + 1; j < len(out); j++ {
			if out[i].RequestedAt > out[j].RequestedAt {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (c *AppServerController) ResolveApprovalDecision(ctx context.Context, id string, decision ApprovalDecision) (string, error) {
	sk := c.sessionKey(ctx)
	id = strings.TrimSpace(id)
	if !decision.Valid() {
		return "", fmt.Errorf("invalid approval decision: %q", decision)
	}
	decisionText := normalizeDecision(decision.String())
	c.stateMu.Lock()
	s := c.getSessionLocked(sk)
	if id == "" {
		if len(s.appByID) == 1 {
			for k := range s.appByID {
				id = k
			}
		} else {
			c.stateMu.Unlock()
			return "", errors.New("approval id is required when multiple requests exist")
		}
	}
	p, ok := s.appByID[id]
	if !ok {
		c.stateMu.Unlock()
		return "", fmt.Errorf("approval not found: %s", id)
	}
	c.stateMu.Unlock()

	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      p.ServerID,
	}
	resp["result"] = c.approvalDecisionResult(p.Method, decisionText)
	if err := c.send(resp); err != nil {
		return "", err
	}
	// Remove local pending approval only after response is successfully sent.
	c.stateMu.Lock()
	s = c.getSessionLocked(sk)
	delete(s.appByID, id)
	delete(s.approvals, id)
	c.stateMu.Unlock()

	// Best-effort: for positive approvals, wait briefly for resumed turn result
	// so Telegram can show completion without forcing a separate user prompt.
	if (decisionText == "accept" || decisionText == "acceptForSession") && strings.TrimSpace(p.TurnID) != "" {
		waitCtx, cancel := context.WithTimeout(context.Background(), approvalResumeTimeoutFromEnv())
		out, err := c.waitTurn(waitCtx, sk, p.TurnID)
		cancel()
		if err == nil && out.Err == nil {
			txt := strings.TrimSpace(out.Content)
			if txt != "" {
				return txt, nil
			}
		}
		// NOTE(fixlist#F001): this is best-effort only. If completion arrives after timeout,
		// Telegram still needs a separate follow-up request to fetch/continue naturally.
	}

	switch decisionText {
	case "accept", "acceptForSession":
		return "approved: " + id + " (" + decisionText + ")", nil
	case "cancel":
		return "canceled: " + id, nil
	default:
		return "denied: " + id, nil
	}
}

func approvalResumeTimeoutFromEnv() time.Duration {
	sec := envInt("MANTISX_APPROVAL_RESUME_WAIT_SEC", 15)
	if sec < 3 {
		sec = 3
	}
	if sec > 30 {
		sec = 30
	}
	return time.Duration(sec) * time.Second
}

func (c *AppServerController) GetProgress(ctx context.Context) (ProgressState, error) {
	sk := c.sessionKey(ctx)
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	s := c.getSessionLocked(sk)
	best := &appTurnTracker{}
	for _, t := range s.turns {
		if t == nil {
			continue
		}
		if t.lastEventAt.After(best.lastEventAt) {
			best = t
		}
	}
	// Prefer a recent tracker that has richer task hints when latest tracker is sparse.
	if strings.TrimSpace(best.task) == "" {
		cutoff := time.Now().Add(-20 * time.Second)
		for _, t := range s.turns {
			if t == nil {
				continue
			}
			if t.lastEventAt.Before(cutoff) {
				continue
			}
			cand := strings.TrimSpace(t.task)
			if cand == "" {
				cand = toProgressTaskHint(t.commandBuf.String())
			}
			if cand == "" {
				cand = toProgressTaskHint(t.assistantBuf.String())
			}
			if cand != "" {
				best = t
				break
			}
		}
	}
	phase := strings.TrimSpace(best.phase)
	if len(s.approvals) > 0 {
		phase = appPhaseApproval
	}
	if phase == "" {
		phase = appPhaseThinking
	}
	task := strings.TrimSpace(best.task)
	if task == "" {
		switch phase {
		case appPhaseToolRunning:
			task = toProgressTaskHint(best.commandBuf.String())
		case appPhaseThinking, appPhaseFinalAnswer:
			task = toProgressTaskHint(best.commandBuf.String())
			if task == "" {
				task = toProgressTaskHint(best.assistantBuf.String())
			}
		}
	}
	return ProgressState{
		Phase: phase,
		Task:  task,
	}, nil
}

func normalizeDecision(decision string) string {
	d := strings.ToLower(strings.TrimSpace(decision))
	switch d {
	case "accept", "approve", "approved", "1":
		return "accept"
	case "acceptforsession", "accept_for_session", "session", "2":
		return "acceptForSession"
	case "cancel", "abort", "4":
		return "cancel"
	default:
		return "decline"
	}
}

func (c *AppServerController) approvalDecisionResult(method string, decision string) map[string]interface{} {
	switch method {
	case "item/commandExecution/requestApproval":
		switch decision {
		case "accept":
			return map[string]interface{}{"decision": "accept"}
		case "acceptForSession":
			return map[string]interface{}{"decision": "acceptForSession"}
		case "cancel":
			return map[string]interface{}{"decision": "cancel"}
		default:
			return map[string]interface{}{"decision": "decline"}
		}
	case "item/fileChange/requestApproval":
		switch decision {
		case "accept":
			return map[string]interface{}{"decision": "accept"}
		case "acceptForSession":
			return map[string]interface{}{"decision": "acceptForSession"}
		case "cancel":
			return map[string]interface{}{"decision": "cancel"}
		default:
			return map[string]interface{}{"decision": "decline"}
		}
	case "item/permissions/requestApproval":
		if decision == "acceptForSession" || decision == "accept" {
			return map[string]interface{}{
				"permissions": map[string]interface{}{},
				"scope":       "session",
			}
		}
		return map[string]interface{}{
			"permissions": map[string]interface{}{},
			"scope":       "turn",
		}
	case "execCommandApproval":
		if decision == "acceptForSession" {
			return map[string]interface{}{"decision": "approved_for_session"}
		}
		if decision == "accept" {
			return map[string]interface{}{"decision": "approved"}
		}
		if decision == "cancel" {
			return map[string]interface{}{"decision": "abort"}
		}
		return map[string]interface{}{"decision": "denied"}
	case "applyPatchApproval":
		if decision == "acceptForSession" {
			return map[string]interface{}{"decision": "approved_for_session"}
		}
		if decision == "accept" {
			return map[string]interface{}{"decision": "approved"}
		}
		if decision == "cancel" {
			return map[string]interface{}{"decision": "abort"}
		}
		return map[string]interface{}{"decision": "denied"}
	default:
		if decision == "acceptForSession" {
			return map[string]interface{}{"decision": "acceptForSession"}
		}
		if decision == "accept" {
			return map[string]interface{}{"decision": "accept"}
		}
		if decision == "cancel" {
			return map[string]interface{}{"decision": "cancel"}
		}
		return map[string]interface{}{"decision": "decline"}
	}
}

func (c *AppServerController) SubmitPrompt(ctx context.Context, prompt string) (string, error) {
	sk := c.sessionKey(ctx)
	p := strings.TrimSpace(prompt)
	if p == "" {
		return "", errors.New("empty prompt")
	}
	if err := c.ensureStarted(ctx); err != nil {
		return "", err
	}
	if err := c.ensureInitialized(ctx); err != nil {
		return "", fmt.Errorf("app-server initialize: %w", err)
	}
	if err := c.ensureThread(ctx, sk); err != nil {
		return "", err
	}

	c.procMu.Lock()
	p = withWorkspacePrefix(c.workspaceDir, p)
	approvalEnabled := c.approvalEnabled
	c.procMu.Unlock()
	c.stateMu.Lock()
	s := c.getSessionLocked(sk)
	threadID := s.threadID
	c.stateMu.Unlock()

	resp, err := c.request(ctx, "turn/start", map[string]interface{}{
		"threadId": threadID,
		"input": []map[string]interface{}{
			{
				"type":          "text",
				"text":          p,
				"text_elements": []interface{}{},
			},
		},
	})
	if err != nil {
		return "", err
	}
	turnID := readStringFromMap(resp, "turn", "id")
	if turnID == "" {
		return "", errors.New("turn/start: missing turn id")
	}
	c.stateMu.Lock()
	s = c.getSessionLocked(sk)
	t := s.turns[turnID]
	if t == nil {
		t = &appTurnTracker{}
		s.turns[turnID] = t
	}
	if strings.TrimSpace(t.task) == "" {
		t.task = shortText(p, 120)
	}
	if strings.TrimSpace(t.phase) == "" {
		t.phase = appPhaseThinking
	}
	t.lastEventAt = time.Now()
	c.stateMu.Unlock()

	result, err := c.waitTurn(ctx, sk, turnID)
	if err != nil {
		return "", err
	}
	if result.Approval != nil && approvalEnabled {
		return "", &ApprovalRequiredError{Request: *result.Approval}
	}
	if result.Err != nil {
		return "", result.Err
	}
	content := strings.TrimSpace(result.Content)
	if content == "" {
		content = "No response received. Please try again."
	}
	return content, nil
}

func (c *AppServerController) waitTurn(ctx context.Context, sessionKey string, turnID string) (appTurnResult, error) {
	ch := make(chan appTurnResult, 1)
	c.stateMu.Lock()
	s := c.getSessionLocked(sessionKey)
	threadID := s.threadID
	t := s.turns[turnID]
	if t == nil {
		t = &appTurnTracker{}
		s.turns[turnID] = t
	}
	if t.completed != nil {
		done := *t.completed
		c.stateMu.Unlock()
		return done, nil
	}
	if t.approval != nil && !t.approvalSent {
		t.approvalSent = true
		done := appTurnResult{Approval: t.approval}
		c.stateMu.Unlock()
		return done, nil
	}
	t.waiter = ch
	c.stateMu.Unlock()

	select {
	case out := <-ch:
		return out, nil
	case <-ctx.Done():
		c.stateMu.Lock()
		if t2 := s.turns[turnID]; t2 != nil && t2.waiter == ch {
			t2.waiter = nil
		}
		c.stateMu.Unlock()
		if threadID != "" {
			interruptCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			// App Server uses turn/interrupt for in-flight turn cancellation.
			_, _ = c.request(interruptCtx, "turn/interrupt", map[string]interface{}{
				"threadId": threadID,
				"turnId":   turnID,
			})
			cancel()
		}
		return appTurnResult{}, ctx.Err()
	}
}

func (c *AppServerController) ensureThread(ctx context.Context, sessionKey string) error {
	c.stateMu.Lock()
	s := c.getSessionLocked(sessionKey)
	if s.threadID != "" {
		c.stateMu.Unlock()
		return nil
	}
	c.stateMu.Unlock()

	approvalPolicy := "never"
	sandboxMode := "danger-full-access"
	c.procMu.Lock()
	if c.approvalEnabled {
		approvalPolicy = "on-request"
	}
	cwd := c.workspaceDir
	c.procMu.Unlock()
	if strings.TrimSpace(cwd) != "" {
		if err := validateWorkspaceDir(cwd); err != nil {
			cwd = ""
		}
	}
	if strings.TrimSpace(cwd) == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	resp, err := c.request(ctx, "thread/start", map[string]interface{}{
		"cwd":                    cwd,
		"approvalPolicy":         approvalPolicy,
		"sandbox":                sandboxMode,
		"experimentalRawEvents":  false,
		"persistExtendedHistory": true,
	})
	if err != nil {
		return err
	}
	threadID := readStringFromMap(resp, "thread", "id")
	if threadID == "" {
		return errors.New("thread/start: missing thread id")
	}
	c.stateMu.Lock()
	s = c.getSessionLocked(sessionKey)
	s.threadID = threadID
	c.threadToSession[threadID] = sessionKey
	c.stateMu.Unlock()
	return nil
}

func (c *AppServerController) ensureInitialized(ctx context.Context) error {
	c.procMu.Lock()
	if c.initialized {
		c.procMu.Unlock()
		return nil
	}
	c.procMu.Unlock()

	_, err := c.request(ctx, "initialize", map[string]interface{}{
		"clientInfo": map[string]interface{}{"name": "mantisx", "version": "0.1"},
		"capabilities": map[string]interface{}{
			"experimentalApi": true,
		},
	})
	if err != nil {
		return err
	}
	_ = c.send(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "initialized",
	})
	c.procMu.Lock()
	c.initialized = true
	c.procMu.Unlock()
	return nil
}

func (c *AppServerController) ensureStarted(ctx context.Context) error {
	c.procMu.Lock()
	defer c.procMu.Unlock()
	if c.started && c.cmd != nil {
		return nil
	}

	cmd := exec.CommandContext(context.Background(), c.CodexExe, c.Args...)
	if strings.TrimSpace(c.CodexCwd) != "" {
		cmd.Dir = c.CodexCwd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return err
	}
	c.debugf("app-server start pid=%d exe=%s args=%v cwd=%s", cmd.Process.Pid, c.CodexExe, c.Args, cmd.Dir)
	c.cmd = cmd
	c.stdin = stdin
	c.readerDone = make(chan struct{})
	c.started = true
	c.initialized = false
	go c.readLoop(stdout, stderr, c.readerDone)
	return nil
}

func (c *AppServerController) shutdownLocked(cause error) {
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
	c.cmd = nil
	c.stdin = nil
	c.started = false
	c.initialized = false
	c.failPending(cause)
	c.stateMu.Lock()
	c.sessions = map[string]*appSessionState{}
	c.threadToSession = map[string]string{}
	c.stateMu.Unlock()
}

func (c *AppServerController) readLoop(stdout io.Reader, stderr io.Reader, done chan struct{}) {
	defer close(done)
	stderrBuf := bufio.NewScanner(stderr)
	stderrBuf.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	go func() {
		for stderrBuf.Scan() {
			c.debugf("stderr: %s", sanitizeLogLine(stderrBuf.Text()))
		}
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		c.debugf("stdout: %s", sanitizeLogLine(line))
		var msg map[string]interface{}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			c.debugf("stdout non-json: %v", err)
			continue
		}
		c.handleIncoming(msg)
	}
	if err := sc.Err(); err != nil {
		c.debugf("stdout scan error: %v", err)
	}
	c.procMu.Lock()
	c.shutdownLocked(errors.New("app-server connection closed"))
	c.procMu.Unlock()
}

func (c *AppServerController) failPending(err error) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if err == nil {
		err = errors.New("connection closed")
	}
	for id, ch := range c.pendingRPC {
		ch <- appRPCResult{err: err}
		close(ch)
		delete(c.pendingRPC, id)
	}
	for _, s := range c.sessions {
		for _, t := range s.turns {
			if t.waiter != nil {
				t.waiter <- appTurnResult{Err: err}
				close(t.waiter)
				t.waiter = nil
			}
		}
	}
}

func (c *AppServerController) handleIncoming(msg map[string]interface{}) {
	method := strings.TrimSpace(anyToString(msg["method"]))
	idv, hasID := msg["id"]
	if method == "" && hasID {
		c.resolvePendingRPC(idv, msg)
		return
	}
	if method != "" && hasID {
		c.handleServerRequest(method, idv, msg)
		return
	}
	if method != "" {
		c.handleNotification(method, msg)
	}
}

func (c *AppServerController) resolvePendingRPC(id interface{}, msg map[string]interface{}) {
	key := anyToString(id)
	if key == "" {
		return
	}
	c.stateMu.Lock()
	ch := c.pendingRPC[key]
	if ch != nil {
		delete(c.pendingRPC, key)
	}
	c.stateMu.Unlock()
	if ch == nil {
		return
	}
	if remoteErr := readRPCError(msg); remoteErr != nil {
		ch <- appRPCResult{err: remoteErr}
		close(ch)
		return
	}
	if result, ok := msg["result"].(map[string]interface{}); ok {
		ch <- appRPCResult{result: result}
	} else {
		ch <- appRPCResult{result: map[string]interface{}{}}
	}
	close(ch)
}

func (c *AppServerController) handleServerRequest(method string, id interface{}, msg map[string]interface{}) {
	params, _ := msg["params"].(map[string]interface{})
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "execCommandApproval", "applyPatchApproval":
		req := c.registerApproval(method, id, params)
		turnID := strings.TrimSpace(anyToString(params["turnId"]))
		threadID := strings.TrimSpace(anyToString(params["threadId"]))
		if turnID != "" {
			c.markTurnApproval(threadID, turnID, &req)
		}
	default:
		_ = c.send(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]interface{}{
				"code":    -32000,
				"message": "unsupported request in mantisx bridge: " + method,
			},
		})
	}
}

func (c *AppServerController) registerApproval(method string, serverID interface{}, params map[string]interface{}) ApprovalRequest {
	summary := buildApprovalSummary(method, params)
	threadID := strings.TrimSpace(anyToString(params["threadId"]))
	kind := approvalKindFromMethod(method)
	command, cwd := extractApprovalCommandAndCwd(params)
	files := extractApprovalFiles(params)
	rawPayload := encodeCompactJSON(params)
	requestID := strings.TrimSpace(anyToString(serverID))
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	session := c.findSessionByThreadLocked(threadID)
	sessionKey := "default"
	if session == nil {
		session = c.getSessionLocked("default")
	} else {
		if k := c.threadToSession[threadID]; k != "" {
			sessionKey = k
		}
	}
	session.approvalSeq++
	localID := fmt.Sprintf("apr-%04d", session.approvalSeq)
	req := ApprovalRequest{
		ID:          localID,
		SessionKey:  sessionKey,
		Kind:        kind,
		Summary:     shortText(summary, 500),
		Command:     shortText(command, 500),
		Cwd:         shortText(cwd, 500),
		Files:       files,
		RequestID:   requestID,
		RequestedAt: time.Now().Format("2006-01-02 15:04:05"),
		RawPayload:  shortText(rawPayload, 2000),
	}
	session.approvals[localID] = req
	session.appByID[localID] = appPendingApproval{
		LocalID:   localID,
		ServerID:  serverID,
		Method:    method,
		TurnID:    strings.TrimSpace(anyToString(params["turnId"])),
		ThreadID:  threadID,
		Requested: req,
	}
	return req
}

func approvalKindFromMethod(method string) ApprovalKind {
	switch method {
	case "item/commandExecution/requestApproval", "execCommandApproval":
		return ApprovalKindCommand
	case "item/fileChange/requestApproval", "applyPatchApproval":
		return ApprovalKindFileChange
	default:
		return ApprovalKindUnknown
	}
}

func extractApprovalCommandAndCwd(params map[string]interface{}) (string, string) {
	if params == nil {
		return "", ""
	}
	command := strings.TrimSpace(anyToString(params["command"]))
	cwd := strings.TrimSpace(anyToString(params["cwd"]))
	if item, ok := params["item"].(map[string]interface{}); ok && item != nil {
		if command == "" {
			command = strings.TrimSpace(anyToString(item["command"]))
		}
		if cwd == "" {
			cwd = strings.TrimSpace(anyToString(item["cwd"]))
		}
	}
	return command, cwd
}

func extractApprovalFiles(params map[string]interface{}) []string {
	if params == nil {
		return nil
	}
	out := map[string]struct{}{}
	appendPath := func(v interface{}) {
		p := strings.TrimSpace(anyToString(v))
		if p != "" {
			out[p] = struct{}{}
		}
	}
	if item, ok := params["item"].(map[string]interface{}); ok && item != nil {
		appendPath(item["path"])
		if changes, ok := item["changes"].([]interface{}); ok {
			for _, ch := range changes {
				if m, ok := ch.(map[string]interface{}); ok {
					appendPath(m["path"])
					appendPath(m["filePath"])
				}
			}
		}
	}
	if changes, ok := params["changes"].([]interface{}); ok {
		for _, ch := range changes {
			if m, ok := ch.(map[string]interface{}); ok {
				appendPath(m["path"])
				appendPath(m["filePath"])
			} else {
				appendPath(ch)
			}
		}
	}
	if path, ok := params["path"]; ok {
		appendPath(path)
	}
	files := make([]string, 0, len(out))
	for p := range out {
		files = append(files, p)
	}
	sort.Strings(files)
	return files
}

func encodeCompactJSON(v interface{}) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func (c *AppServerController) markTurnApproval(threadID, turnID string, req *ApprovalRequest) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	session := c.findSessionByThreadLocked(threadID)
	if session == nil {
		session = c.getSessionLocked("default")
	}
	t := session.turns[turnID]
	if t == nil {
		t = &appTurnTracker{}
		session.turns[turnID] = t
	}
	t.phase = appPhaseApproval
	t.lastEventAt = time.Now()
	t.approval = req
	if t.waiter != nil && !t.approvalSent {
		t.approvalSent = true
		t.waiter <- appTurnResult{Approval: req}
		close(t.waiter)
		t.waiter = nil
	}
}

func (c *AppServerController) handleNotification(method string, msg map[string]interface{}) {
	params, _ := msg["params"].(map[string]interface{})
	switch method {
	case "thread/started":
		threadID := readStringFromMap(params, "thread", "id")
		if threadID != "" {
			// Mapping is set during thread/start response path.
			c.debugf("thread started event thread=%s", threadID)
		}
	case "turn/started":
		threadID := strings.TrimSpace(anyToString(params["threadId"]))
		turnObj, _ := params["turn"].(map[string]interface{})
		turnID := strings.TrimSpace(anyToString(turnObj["id"]))
		if turnID == "" {
			return
		}
		c.stateMu.Lock()
		session := c.findSessionByThreadLocked(threadID)
		if session == nil {
			session = c.getSessionLocked("default")
		}
		if _, ok := session.turns[turnID]; !ok {
			session.turns[turnID] = &appTurnTracker{}
		}
		session.turns[turnID].phase = appPhaseThinking
		session.turns[turnID].lastEventAt = time.Now()
		c.stateMu.Unlock()
	case "item/started":
		threadID := strings.TrimSpace(anyToString(params["threadId"]))
		turnID := strings.TrimSpace(anyToString(params["turnId"]))
		if turnID == "" {
			return
		}
		item, _ := params["item"].(map[string]interface{})
		itemType := normalizeItemType(anyToString(item["type"]))
		itemPhase := normalizeItemPhase(params, item)
		c.stateMu.Lock()
		session := c.findSessionByThreadLocked(threadID)
		if session == nil {
			session = c.getSessionLocked("default")
		}
		t := session.turns[turnID]
		if t == nil {
			t = &appTurnTracker{}
			session.turns[turnID] = t
		}
		switch itemType {
		case "reasoning":
			t.phase = appPhaseThinking
		case "commandexecution":
			t.phase = appPhaseToolRunning
			task := toProgressTaskHint(readStringFromMap(item, "command"))
			if strings.TrimSpace(task) != "" {
				t.task = task
				t.taskUpdatedAt = time.Now()
			}
		case "agentmessage":
			if itemPhase == "commentary" {
				t.phase = appPhaseThinking
				if strings.TrimSpace(t.task) == "" {
					t.task = "analysis"
				}
			} else {
				t.phase = appPhaseFinalAnswer
			}
		default:
			t.phase = appPhaseIgnore
		}
		t.lastEventAt = time.Now()
		c.stateMu.Unlock()
	case "item/agentMessage/delta":
		threadID := strings.TrimSpace(anyToString(params["threadId"]))
		turnID := strings.TrimSpace(anyToString(params["turnId"]))
		delta := anyToString(params["delta"])
		if turnID == "" || delta == "" {
			return
		}
		item, _ := params["item"].(map[string]interface{})
		itemPhase := normalizeItemPhase(params, item)
		c.stateMu.Lock()
		session := c.findSessionByThreadLocked(threadID)
		if session == nil {
			session = c.getSessionLocked("default")
		}
		t := session.turns[turnID]
		if t == nil {
			t = &appTurnTracker{}
			session.turns[turnID] = t
		}
		if itemPhase != "commentary" {
			appendLimited(&t.assistantBuf, delta, assistantBufferMaxRun)
			if t.phase == "" || t.phase == appPhaseIgnore {
				t.phase = appPhaseThinking
			}
			if t.phase == appPhaseThinking {
				updateTurnTaskFromDelta(t, delta)
			}
		} else {
			t.phase = appPhaseThinking
			updateTurnTaskFromDelta(t, delta)
		}
		t.lastEventAt = time.Now()
		c.stateMu.Unlock()
	case "item/commandExecution/outputDelta":
		threadID := strings.TrimSpace(anyToString(params["threadId"]))
		turnID := strings.TrimSpace(anyToString(params["turnId"]))
		delta := anyToString(params["delta"])
		if turnID == "" || delta == "" {
			return
		}
		c.stateMu.Lock()
		session := c.findSessionByThreadLocked(threadID)
		if session == nil {
			session = c.getSessionLocked("default")
		}
		t := session.turns[turnID]
		if t == nil {
			t = &appTurnTracker{}
			session.turns[turnID] = t
		}
		appendLimited(&t.commandBuf, delta, commandBufferMaxRun)
		t.phase = appPhaseToolRunning
		updateTurnTaskFromDelta(t, delta)
		if strings.TrimSpace(t.task) == "" {
			t.task = "명령 실행 중"
		}
		t.lastEventAt = time.Now()
		c.stateMu.Unlock()
	case "item/completed":
		threadID := strings.TrimSpace(anyToString(params["threadId"]))
		turnID := strings.TrimSpace(anyToString(params["turnId"]))
		if turnID == "" {
			return
		}
		item, _ := params["item"].(map[string]interface{})
		itemType := normalizeItemType(anyToString(item["type"]))
		itemPhase := normalizeItemPhase(params, item)
		c.stateMu.Lock()
		session := c.findSessionByThreadLocked(threadID)
		if session == nil {
			session = c.getSessionLocked("default")
		}
		t := session.turns[turnID]
		if t == nil {
			t = &appTurnTracker{}
			session.turns[turnID] = t
		}
		if itemType == "agentmessage" && itemPhase != "commentary" {
			txt := extractAgentMessageText(item)
			if strings.TrimSpace(txt) != "" {
				t.finalText = shortText(strings.TrimSpace(txt), assistantBufferMaxRun)
				t.phase = appPhaseFinalAnswer
			}
		}
		t.lastEventAt = time.Now()
		c.stateMu.Unlock()
	case "turn/completed":
		threadID := strings.TrimSpace(anyToString(params["threadId"]))
		turnObj, _ := params["turn"].(map[string]interface{})
		turnID := strings.TrimSpace(anyToString(turnObj["id"]))
		if turnID == "" {
			return
		}
		status := strings.TrimSpace(anyToString(turnObj["status"]))
		var turnErr error
		if status == "failed" {
			errObj, _ := turnObj["error"].(map[string]interface{})
			msg := anyToString(errObj["message"])
			if strings.TrimSpace(msg) == "" {
				msg = "turn failed"
			}
			turnErr = errors.New(msg)
		}
		c.stateMu.Lock()
		session := c.findSessionByThreadLocked(threadID)
		if session == nil {
			session = c.getSessionLocked("default")
		}
		t := session.turns[turnID]
		if t == nil {
			t = &appTurnTracker{}
			session.turns[turnID] = t
		}
		content := strings.TrimSpace(t.finalText)
		if content == "" {
			content = strings.TrimSpace(t.assistantBuf.String())
		}
		t.phase = appPhaseFinalAnswer
		t.lastEventAt = time.Now()
		out := appTurnResult{Content: content, Err: turnErr}
		t.completed = &out
		if t.waiter != nil {
			t.waiter <- out
			close(t.waiter)
			t.waiter = nil
		}
		c.stateMu.Unlock()
	}
}

func appendLimited(sb *strings.Builder, text string, maxRunes int) {
	if sb == nil || text == "" || maxRunes <= 0 {
		return
	}
	cur := sb.String() + text
	r := []rune(cur)
	if len(r) > maxRunes {
		r = r[len(r)-maxRunes:]
	}
	sb.Reset()
	sb.WriteString(string(r))
}

func normalizeItemType(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "/", "")
	return s
}

func updateTurnTaskFromDelta(t *appTurnTracker, delta string) {
	if t == nil {
		return
	}
	snippet := toProgressTaskHint(delta)
	if strings.TrimSpace(snippet) == "" {
		return
	}
	now := time.Now()
	if strings.TrimSpace(t.task) == "" || now.Sub(t.taskUpdatedAt) >= 2*time.Second {
		t.task = snippet
		t.taskUpdatedAt = now
	}
}

func toProgressTaskHint(s string) string {
	v := strings.TrimSpace(s)
	if v == "" {
		return ""
	}
	v = sanitizeMCPText(v)
	v = strings.ReplaceAll(v, "\r\n", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	v = strings.ReplaceAll(v, "\t", " ")
	v = strings.TrimSpace(v)
	v = strings.Trim(v, "`\"'[]()")
	v = shortText(v, 80)
	if strings.TrimSpace(v) == "" {
		return ""
	}
	if kw := extractProgressKeyword(v); kw != "" {
		return kw
	}
	l := strings.ToLower(v)
	switch {
	case strings.Contains(l, "read"), strings.Contains(l, "docs"), strings.Contains(v, "문서"):
		return "문서 확인 중"
	case strings.Contains(l, "test"), strings.Contains(l, "pytest"), strings.Contains(l, "go test"), strings.Contains(l, "cargo test"):
		return "테스트 실행 중"
	case strings.Contains(l, "build"), strings.Contains(v, "빌드"):
		return "빌드 확인 중"
	case strings.Contains(l, "server"), strings.Contains(v, "서버"):
		return "서버 점검 중"
	case strings.Contains(l, "config"), strings.Contains(v, "설정"):
		return "설정 확인 중"
	default:
		return v
	}
}

func extractProgressKeyword(s string) string {
	v := strings.TrimSpace(s)
	if v == "" {
		return ""
	}
	// Prefer known App Server / MCP method hints that are meaningful in Telegram progress.
	keywords := []string{
		"gitDiffToRemote",
		"fuzzyFileSearch/sessionUpdate",
		"fuzzyFileSearch/sessionStart",
		"fuzzyFileSearch/sessionStop",
		"fuzzyFileSearch",
		"getConversationSummary",
		"thread/backgroundTerminals/clean",
		"thread/shellCommand",
		"thread/compact/start",
		"thread/rollback",
		"thread/read",
		"thread/list",
		"thread/loaded/list",
		"turn/steer",
		"turn/interrupt",
		"turn/start",
		"command/exec/terminate",
		"command/exec/resize",
		"command/exec",
		"fs/readDirectory",
		"fs/getMetadata",
		"fs/createDirectory",
		"fs/readFile",
		"fs/copy",
		"model/list",
		"config/read",
		"skills/list",
		"plugin/list",
		"mcpServerStatus/list",
		"review/start",
	}
	lv := strings.ToLower(v)
	for _, k := range keywords {
		if strings.Contains(lv, strings.ToLower(k)) {
			return k
		}
	}
	// Fallback: pick first token that looks like a method or command path.
	fields := strings.FieldsFunc(v, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ',', ';', '"', '\'', '(', ')', '[', ']', '{', '}', '<', '>':
			return true
		default:
			return false
		}
	})
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if len(f) < 4 {
			continue
		}
		lf := strings.ToLower(f)
		if strings.Contains(lf, "write") || strings.Contains(lf, "delete") || strings.Contains(lf, "remove") || strings.HasPrefix(lf, "rm") {
			continue
		}
		if strings.Contains(f, "/") || strings.Contains(f, "\\") || strings.Contains(strings.ToLower(f), "git") {
			return shortText(f, 48)
		}
	}
	return ""
}

func normalizeItemPhase(params map[string]interface{}, item map[string]interface{}) string {
	phase := strings.TrimSpace(anyToString(params["itemPhase"]))
	if phase == "" {
		phase = strings.TrimSpace(anyToString(params["phase"]))
	}
	if phase == "" && item != nil {
		phase = strings.TrimSpace(anyToString(item["phase"]))
	}
	return strings.ToLower(phase)
}

func extractAgentMessageText(item map[string]interface{}) string {
	if item == nil {
		return ""
	}
	content, _ := item["content"].([]interface{})
	var b strings.Builder
	for _, c := range content {
		obj, _ := c.(map[string]interface{})
		if obj == nil {
			continue
		}
		typ := normalizeItemType(anyToString(obj["type"]))
		if typ == "outputtext" || typ == "text" {
			txt := anyToString(obj["text"])
			if strings.TrimSpace(txt) == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(txt)
		}
	}
	return b.String()
}

func (c *AppServerController) request(ctx context.Context, method string, params map[string]interface{}) (map[string]interface{}, error) {
	id := strconv.FormatInt(atomic.AddInt64(&c.nextID, 1), 10)
	ch := make(chan appRPCResult, 1)
	c.stateMu.Lock()
	c.pendingRPC[id] = ch
	c.stateMu.Unlock()

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	c.debugf("request id=%s method=%s", id, method)
	if err := c.send(req); err != nil {
		c.stateMu.Lock()
		delete(c.pendingRPC, id)
		c.stateMu.Unlock()
		return nil, err
	}
	select {
	case out := <-ch:
		if out.err != nil {
			c.debugf("response id=%s method=%s err=%v", id, method, out.err)
		} else {
			c.debugf("response id=%s method=%s ok", id, method)
		}
		return out.result, out.err
	case <-ctx.Done():
		c.stateMu.Lock()
		delete(c.pendingRPC, id)
		c.stateMu.Unlock()
		c.debugf("request timeout id=%s method=%s err=%v", id, method, ctx.Err())
		return nil, ctx.Err()
	}
}

func (c *AppServerController) send(obj map[string]interface{}) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	c.procMu.Lock()
	stdin := c.stdin
	c.procMu.Unlock()
	if stdin == nil {
		return errors.New("app-server not running")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = stdin.Write(append(b, '\n'))
	if err != nil {
		c.debugf("send error: %v", err)
	}
	return err
}

func (c *AppServerController) debugf(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	logStdIOToConsole := envBool("MANTISX_APP_SERVER_STDIO_CONSOLE", false)
	isStdIO := strings.HasPrefix(msg, "stdout:") || strings.HasPrefix(msg, "stderr:")
	if !isStdIO || logStdIOToConsole {
		log.Printf("[appserver] %s", msg)
	}
	logPath := strings.TrimSpace(os.Getenv("MANTISX_APP_SERVER_LOG"))
	if logPath == "" {
		if wd, err := os.Getwd(); err == nil {
			logPath = filepath.Join(wd, "state", "app_server_controller.log")
		}
	}
	if logPath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(time.Now().Format("2006-01-02 15:04:05.000") + " " + msg + "\n")
}

func sanitizeLogLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1500 {
		return s[:1500] + "..."
	}
	return s
}

func readRPCError(msg map[string]interface{}) error {
	errObj, _ := msg["error"].(map[string]interface{})
	if errObj == nil {
		return nil
	}
	code := 0
	switch v := errObj["code"].(type) {
	case float64:
		code = int(v)
	case int:
		code = v
	}
	message := anyToString(errObj["message"])
	return &JSONRPCRemoteError{Code: code, Message: message, Data: errObj["data"]}
}

func buildApprovalSummary(method string, params map[string]interface{}) string {
	reason := strings.TrimSpace(anyToString(params["reason"]))
	command := strings.TrimSpace(anyToString(params["command"]))
	if command == "" {
		if arr, ok := params["command"].([]interface{}); ok && len(arr) > 0 {
			parts := make([]string, 0, len(arr))
			for _, v := range arr {
				parts = append(parts, strings.TrimSpace(anyToString(v)))
			}
			command = strings.TrimSpace(strings.Join(parts, " "))
		}
	}
	cwd := strings.TrimSpace(anyToString(params["cwd"]))
	var b strings.Builder
	b.WriteString("approval request")
	if method != "" {
		b.WriteString(" [")
		b.WriteString(method)
		b.WriteString("]")
	}
	if reason != "" {
		b.WriteString("\nreason: ")
		b.WriteString(reason)
	}
	if command != "" {
		b.WriteString("\ncommand: ")
		b.WriteString(command)
	}
	if cwd != "" {
		b.WriteString("\ncwd: ")
		b.WriteString(cwd)
	}
	return b.String()
}

func readStringFromMap(obj map[string]interface{}, keys ...string) string {
	cur := obj
	for i, k := range keys {
		if cur == nil {
			return ""
		}
		v, ok := cur[k]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			return strings.TrimSpace(anyToString(v))
		}
		next, _ := v.(map[string]interface{})
		cur = next
	}
	return ""
}

func anyToString(v interface{}) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return fmt.Sprintf("%v", v)
	}
}
