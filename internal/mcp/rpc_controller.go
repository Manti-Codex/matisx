package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultCodexDaemonAddr = "127.0.0.1:17997"

var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
var ansiOrphanSGRRE = regexp.MustCompile(`\[(?:\d{1,3}(?:;\d{1,3})*)m`)

type RPCController struct {
	Addr   string
	IdleMS int
	MaxMS  int

	InitIdleMS int
	InitMaxMS  int

	workspaceDir string

	mu    sync.Mutex
	store SessionStore

	approvalEnabled bool
}

type SessionState struct {
	Initialized  bool
	ThreadID     string
	ReqID        int
	Approvals    map[string]ApprovalRequest
	ApprovalSeq  int64
	LastActivity time.Time
}

type MethodUnavailableError struct {
	Method string
	Cause  error
}

func (e *MethodUnavailableError) Error() string {
	if e == nil {
		return "method unavailable"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s unavailable: %v", e.Method, e.Cause)
	}
	return fmt.Sprintf("%s unavailable", e.Method)
}

func IsMethodUnavailable(err error) bool {
	var target *MethodUnavailableError
	return errors.As(err, &target)
}

type ApprovalRequiredError struct {
	Request ApprovalRequest
}

func (e *ApprovalRequiredError) Error() string {
	if e == nil {
		return "approval required"
	}
	return "approval required: " + e.Request.ID
}

func AsApprovalRequired(err error) (*ApprovalRequiredError, bool) {
	var target *ApprovalRequiredError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

type JSONRPCRemoteError struct {
	Code    int
	Message string
	Data    interface{}
	Method  string
}

func (e *JSONRPCRemoteError) Error() string {
	if e == nil {
		return "json-rpc remote error"
	}
	if e.Method != "" {
		return fmt.Sprintf("json-rpc remote error method=%s code=%d message=%s", e.Method, e.Code, e.Message)
	}
	return fmt.Sprintf("json-rpc remote error code=%d message=%s", e.Code, e.Message)
}

func IsJSONRPCRemoteError(err error) bool {
	var target *JSONRPCRemoteError
	return errors.As(err, &target)
}

type streamEventKind string

const (
	streamEventInit   streamEventKind = "init"
	streamEventText   streamEventKind = "text"
	streamEventTool   streamEventKind = "tool"
	streamEventStatus streamEventKind = "status"
	streamEventResult streamEventKind = "result"
	streamEventError  streamEventKind = "error"
)

type streamEvent struct {
	Kind      streamEventKind
	Text      string
	SessionID string
	ToolName  string
	Status    string
	Usage     map[string]interface{}
	RemoteErr *JSONRPCRemoteError
}

type rpcEmptyArgs struct{}

type rpcSubmitArgs struct {
	Prompt string
	IdleMS int
	MaxMS  int
}

type rpcSubmitReply struct {
	OK    bool
	Lines []string
	Err   string
}

type rpcControlReply struct {
	OK  bool
	Msg string
	Err string
}

type rpcResolveApprovalArgs struct {
	SessionKey string `json:"session_key"`
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"` // approve|deny
}

type rpcResolveApprovalReply struct {
	OK  bool   `json:"ok"`
	Msg string `json:"msg"`
	Err string `json:"err"`
}

func NewRPCControllerFromEnv() *RPCController {
	idle := envInt("MANTISX_CODEX_IDLE_MS", 20000)
	max := envInt("MANTISX_CODEX_MAX_MS", 150000)
	initIdle := envInt("MANTISX_MCP_INIT_IDLE_MS", 4000)
	initMax := envInt("MANTISX_MCP_INIT_MAX_MS", 240000)
	if idle < 4000 {
		idle = 4000
	}
	if initIdle < idle {
		initIdle = idle
	}
	if initMax < max {
		initMax = max
	}
	return &RPCController{
		Addr:            codexDaemonAddrFromEnv(),
		IdleMS:          idle,
		MaxMS:           max,
		InitIdleMS:      initIdle,
		InitMaxMS:       initMax,
		workspaceDir:    strings.TrimSpace(os.Getenv("MANTISX_WORKSPACE_DIR")),
		store:           NewInMemorySessionStore(),
		approvalEnabled: envBool("MANTISX_APPROVAL_ENABLED", false),
	}
}

func (c *RPCController) sessionKey(ctx context.Context) string {
	return SessionKeyFromContext(ctx)
}

func (c *RPCController) getSessionLocked(key string) *SessionState {
	if c.store == nil {
		c.store = NewInMemorySessionStore()
	}
	s, _ := c.store.Get(key)
	if s == nil {
		s = &SessionState{
			ReqID:     1000,
			Approvals: map[string]ApprovalRequest{},
		}
		c.store.Put(key, s)
	}
	s.LastActivity = time.Now()
	return s
}

func (c *RPCController) SetWorkspaceDir(dir string) {
	c.mu.Lock()
	c.workspaceDir = strings.TrimSpace(dir)
	c.mu.Unlock()
}

func (c *RPCController) SetAddr(addr string) {
	c.mu.Lock()
	c.Addr = strings.TrimSpace(addr)
	c.mu.Unlock()
}

func (c *RPCController) ClearSession(ctx context.Context) error {
	var rep rpcControlReply
	if err := c.call(ctx, "Daemon.Clear", rpcEmptyArgs{}, &rep); err != nil {
		if isRPCMethodNotFound(err) {
			return &MethodUnavailableError{Method: "Daemon.Clear", Cause: err}
		}
		return err
	}
	if !rep.OK {
		if rep.Err != "" {
			return errors.New(rep.Err)
		}
		return errors.New("daemon clear not ok")
	}
	c.mu.Lock()
	sk := c.sessionKey(ctx)
	s := c.getSessionLocked(sk)
	s.Initialized = false
	s.ThreadID = ""
	s.ReqID = 1000
	s.Approvals = map[string]ApprovalRequest{}
	c.mu.Unlock()
	return nil
}

func (c *RPCController) StopDaemon(ctx context.Context) error {
	var rep rpcControlReply
	if err := c.call(ctx, "Daemon.Stop", rpcEmptyArgs{}, &rep); err != nil {
		if isRPCMethodNotFound(err) {
			return &MethodUnavailableError{Method: "Daemon.Stop", Cause: err}
		}
		return err
	}
	if !rep.OK {
		if rep.Err != "" {
			return errors.New(rep.Err)
		}
		return errors.New("daemon stop not ok")
	}
	c.mu.Lock()
	c.store = NewInMemorySessionStore()
	c.mu.Unlock()
	return nil
}

func (c *RPCController) ListApprovals(ctx context.Context) ([]ApprovalRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.getSessionLocked(c.sessionKey(ctx))
	list := make([]ApprovalRequest, 0, len(s.Approvals))
	for _, v := range s.Approvals {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].RequestedAt < list[j].RequestedAt })
	return list, nil
}

func (c *RPCController) ApprovalEnabled(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.approvalEnabled
}

func (c *RPCController) SetApprovalEnabled(ctx context.Context, enabled bool) (string, error) {
	c.mu.Lock()
	c.approvalEnabled = enabled
	c.mu.Unlock()
	if enabled {
		return "approval mode enabled", nil
	}
	return "approval mode disabled", nil
}

func (c *RPCController) ResolveApprovalDecision(ctx context.Context, id string, decision ApprovalDecision) (string, error) {
	if !decision.Valid() {
		return "", fmt.Errorf("invalid approval decision: %q", decision)
	}
	sk := c.sessionKey(ctx)
	id = strings.TrimSpace(id)
	c.mu.Lock()
	s := c.getSessionLocked(sk)
	if id == "" {
		if len(s.Approvals) == 1 {
			for k := range s.Approvals {
				id = k
				break
			}
		} else {
			c.mu.Unlock()
			return "", errors.New("approval id is required when multiple requests exist")
		}
	}
	req, ok := s.Approvals[id]
	c.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("approval not found: %s", id)
	}

	decisionText := "deny"
	switch decision {
	case ApprovalDecisionAccept:
		decisionText = "approve"
	case ApprovalDecisionAcceptForSession:
		decisionText = "approve_for_session"
	case ApprovalDecisionCancel:
		decisionText = "cancel"
	default:
		decisionText = "deny"
	}

	// 1) Preferred: protocol-level approval resolution.
	var rep rpcResolveApprovalReply
	err := c.call(ctx, "Daemon.ResolveApproval", rpcResolveApprovalArgs{
		SessionKey: req.SessionKey,
		ApprovalID: req.ID,
		Decision:   decisionText,
	}, &rep)
	if err == nil {
		if !rep.OK {
			if strings.TrimSpace(rep.Err) != "" {
				return "", errors.New(strings.TrimSpace(rep.Err))
			}
			return "", errors.New("approval resolve not ok")
		}
		c.mu.Lock()
		s2 := c.getSessionLocked(sk)
		delete(s2.Approvals, id)
		c.mu.Unlock()
		if strings.TrimSpace(rep.Msg) != "" {
			return rep.Msg, nil
		}
		switch decision {
		case ApprovalDecisionAccept:
			return "approved: " + id, nil
		case ApprovalDecisionAcceptForSession:
			return "approved for session: " + id, nil
		case ApprovalDecisionCancel:
			return "canceled: " + id, nil
		default:
			return "denied: " + id, nil
		}
	}
	if !isRPCMethodNotFound(err) {
		return "", err
	}

	// 2) Fallback: natural-language continuation for old daemon builds.
	// NOTE(fixlist#F004): legacy fallback is not protocol-level resume and can drift
	// from daemon paused-state semantics. Keep only for backward compatibility.
	return c.resolveApprovalLegacy(ctx, sk, req, decision)
}

// Optional compatibility wrapper during migration.
// Remove after all call sites switch to ResolveApprovalDecision.
func (c *RPCController) ResolveApproval(ctx context.Context, id string, approve bool) (string, error) {
	if approve {
		return c.ResolveApprovalDecision(ctx, id, ApprovalDecisionAccept)
	}
	return c.ResolveApprovalDecision(ctx, id, ApprovalDecisionDecline)
}

func (c *RPCController) resolveApprovalLegacy(
	ctx context.Context,
	sessionKey string,
	req ApprovalRequest,
	decision ApprovalDecision,
) (string, error) {
	var follow string

	switch decision {
	case ApprovalDecisionAccept:
		follow = fmt.Sprintf(
			"Approval decision for request %s.\nSession: %s\nDecision: approve\nKind: %s\nSummary: %s\nContinue the previously paused task accordingly.",
			req.ID, req.SessionKey, req.Kind, req.Summary,
		)
	case ApprovalDecisionAcceptForSession:
		follow = fmt.Sprintf(
			"Approval decision for request %s.\nSession: %s\nDecision: approve_for_session\nKind: %s\nSummary: %s\nTreat this as approval for the current session if supported, and continue the previously paused task accordingly.",
			req.ID, req.SessionKey, req.Kind, req.Summary,
		)
	case ApprovalDecisionCancel:
		follow = fmt.Sprintf(
			"Approval decision for request %s.\nSession: %s\nDecision: cancel\nKind: %s\nSummary: %s\nCancel the previously paused task. Do not continue execution.",
			req.ID, req.SessionKey, req.Kind, req.Summary,
		)
	case ApprovalDecisionDecline:
		follow = fmt.Sprintf(
			"Approval decision for request %s.\nSession: %s\nDecision: deny\nKind: %s\nSummary: %s\nDo not continue the previously paused task.",
			req.ID, req.SessionKey, req.Kind, req.Summary,
		)
	default:
		return "", fmt.Errorf("unsupported approval decision: %q", decision)
	}

	_, err := c.SubmitPrompt(WithSessionKey(ctx, sessionKey), follow)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	latest := c.getSessionLocked(sessionKey)
	delete(latest.Approvals, req.ID)

	switch decision {
	case ApprovalDecisionAccept:
		return "approved: " + req.ID, nil
	case ApprovalDecisionAcceptForSession:
		return "approved for session: " + req.ID, nil
	case ApprovalDecisionCancel:
		return "canceled: " + req.ID, nil
	default:
		return "denied: " + req.ID, nil
	}
}

func (c *RPCController) SubmitPrompt(ctx context.Context, prompt string) (string, error) {
	sk := c.sessionKey(ctx)
	p := strings.TrimSpace(prompt)
	if p == "" {
		return "", errors.New("empty prompt")
	}

	c.mu.Lock()
	s := c.getSessionLocked(sk)
	workspaceDir := c.workspaceDir
	approvalEnabled := c.approvalEnabled
	threadID := s.ThreadID
	c.mu.Unlock()

	if err := c.ensureSessionInitialized(ctx, sk); err != nil {
		if isInitDeadlineExceeded(err) && ctx.Err() == nil {
			// Daemon warm-up/temporary stall path: reset local session state and retry once.
			c.mu.Lock()
			s = c.getSessionLocked(sk)
			s.Initialized = false
			s.ThreadID = ""
			c.mu.Unlock()
			if retryErr := c.ensureSessionInitialized(ctx, sk); retryErr != nil {
				return "", retryErr
			}
			c.mu.Lock()
			s = c.getSessionLocked(sk)
			threadID = s.ThreadID
			c.mu.Unlock()
		} else {
			return "", err
		}
	}
	p = withWorkspacePrefix(workspaceDir, p)

	callMsg := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      c.nextReqIDForSession(sk),
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "codex",
			"arguments": map[string]interface{}{
				"prompt": p,
			},
		},
	}
	if threadID != "" {
		callMsg["params"] = map[string]interface{}{
			"name": "codex-reply",
			"arguments": map[string]interface{}{
				"threadId": threadID,
				"prompt":   p,
			},
		}
	}

	resp, err := c.mcpRequestWithTimingLocked(ctx, callMsg, c.IdleMS, c.MaxMS)
	if err != nil {
		if isNoJSONLineError(err) {
			resp, err = c.mcpRequestWithTimingLocked(ctx, callMsg, c.IdleMS*2, c.MaxMS)
		}
		if err != nil && approvalEnabled {
			if req, ok := c.tryBuildApproval(sk, err, ""); ok {
				return "", &ApprovalRequiredError{Request: req}
			}
		}
		if err != nil {
			return "", err
		}
	}
	if approvalEnabled {
		if req, ok := buildApprovalFromResponse(sk, resp); ok {
			return "", &ApprovalRequiredError{Request: c.storeApprovalLocked(sk, req)}
		}
	}
	threadID, content := extractMCPToolResult(resp)
	if threadID != "" {
		c.mu.Lock()
		s = c.getSessionLocked(sk)
		s.ThreadID = threadID
		c.mu.Unlock()
	}
	content = sanitizeMCPText(content)
	if strings.TrimSpace(content) == "" {
		content = "No response received. Please try again."
	}

	if approvalEnabled {
		if req, ok := c.tryBuildApproval(sk, nil, content); ok {
			return "", &ApprovalRequiredError{Request: req}
		}
	}
	return content, nil
}

func readRemoteError(err error) *JSONRPCRemoteError {
	if err == nil {
		return nil
	}
	var target *JSONRPCRemoteError
	if errors.As(err, &target) {
		return target
	}
	return nil
}

func (c *RPCController) ensureSessionInitialized(ctx context.Context, sessionKey string) error {
	c.mu.Lock()
	s := c.getSessionLocked(sessionKey)
	if s.Initialized {
		c.mu.Unlock()
		return nil
	}
	reqID := s.ReqID
	s.ReqID++
	c.mu.Unlock()
	initMsg := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      reqID,
		"method":  "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]interface{}{
				"name":    "mantisx",
				"version": "0.1",
			},
		},
	}
	_, err := c.mcpRequestWithTimingLocked(ctx, initMsg, c.InitIdleMS, c.InitMaxMS)
	if err != nil && isNoJSONLineError(err) {
		_, err = c.mcpRequestWithTimingLocked(ctx, initMsg, c.InitIdleMS*2, c.InitMaxMS)
	}
	if err != nil && !isInitializeAlreadyCalled(err) {
		return fmt.Errorf("mcp initialize: %w", err)
	}
	initialized := map[string]interface{}{"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]interface{}{}}
	if err := c.mcpNotifyLocked(ctx, initialized); err != nil {
		return fmt.Errorf("mcp initialized notify: %w", err)
	}
	c.mu.Lock()
	s = c.getSessionLocked(sessionKey)
	s.Initialized = true
	c.mu.Unlock()
	return nil
}

func (c *RPCController) mcpRequestWithTimingLocked(ctx context.Context, obj map[string]interface{}, idleMS, maxMS int) (map[string]interface{}, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	reqID := ""
	if v, ok := obj["id"]; ok {
		reqID = fmt.Sprintf("%v", v)
	}
	lines, err := c.submitRawLocked(ctx, string(b), idleMS, maxMS)
	if err != nil {
		return nil, err
	}
	var last map[string]interface{}
	var textParts []string
	var rawTextParts []string
	approvalHint := ""
	threadID := ""
	method, _ := obj["method"].(string)
	for _, ln := range lines {
		if t := extractRawLineText(ln); t != "" {
			rawTextParts = append(rawTextParts, t)
			if approvalHint == "" {
				if summary, ok := detectApprovalSignal(t); ok {
					approvalHint = summary
				}
			}
		}
		m, ok := parseJSONLineMap(ln)
		if !ok {
			continue
		}
		last = m
		for _, ev := range parseStreamEvents(m, method) {
			switch ev.Kind {
			case streamEventInit:
				if ev.SessionID != "" {
					threadID = ev.SessionID
				}
			case streamEventText:
				clean := sanitizeMCPText(ev.Text)
				if strings.TrimSpace(clean) != "" {
					textParts = append(textParts, clean)
					if approvalHint == "" {
						if summary, ok := detectApprovalSignal(clean); ok {
							approvalHint = summary
						}
					}
				}
			case streamEventError:
				if reqID == "" {
					return nil, ev.RemoteErr
				}
				if idv, ok := m["id"]; ok && fmt.Sprintf("%v", idv) == reqID {
					return nil, ev.RemoteErr
				}
			}
		}
		if reqID != "" {
			if idv, ok := m["id"]; ok && fmt.Sprintf("%v", idv) == reqID {
				if _, hasResult := m["result"]; hasResult {
					tid, cc := extractMCPToolResult(m)
					if strings.TrimSpace(cc) != "" {
						return m, nil
					}
					if tid != "" {
						threadID = tid
					}
					// Keep scanning: some daemons emit empty result first and stream text deltas after.
					continue
				}
			}
		}
	}
	if combined := strings.TrimSpace(strings.Join(textParts, "")); combined != "" {
		return map[string]interface{}{"result": map[string]interface{}{"threadId": threadID, "content": combined}}, nil
	}
	if strings.TrimSpace(approvalHint) != "" {
		return map[string]interface{}{"result": map[string]interface{}{"threadId": threadID, "content": approvalHint}}, nil
	}
	if combinedRaw := strings.TrimSpace(strings.Join(rawTextParts, "\n")); combinedRaw != "" {
		return map[string]interface{}{"result": map[string]interface{}{"threadId": threadID, "content": combinedRaw}}, nil
	}
	if remoteErr, ok := parseEnvelopeRemoteError(last, method); ok {
		return nil, remoteErr
	}
	if last != nil {
		return last, nil
	}
	return nil, fmt.Errorf("no json response line (lines=%d)", len(lines))
}

func (c *RPCController) mcpNotifyLocked(ctx context.Context, obj map[string]interface{}) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = c.submitRawLocked(ctx, string(b), 300, 3000)
	return err
}

func (c *RPCController) submitRawLocked(ctx context.Context, prompt string, idleMS, maxMS int) ([]string, error) {
	var rep rpcSubmitReply
	if err := c.call(ctx, "Daemon.Submit", rpcSubmitArgs{Prompt: prompt, IdleMS: idleMS, MaxMS: maxMS}, &rep); err != nil {
		if isRPCMethodNotFound(err) {
			return nil, &MethodUnavailableError{Method: "Daemon.Submit", Cause: err}
		}
		return nil, err
	}
	if !rep.OK {
		if rep.Err != "" {
			return nil, errors.New(rep.Err)
		}
		return nil, errors.New("daemon submit not ok")
	}
	return rep.Lines, nil
}

func (c *RPCController) nextReqIDForSession(sessionKey string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.getSessionLocked(sessionKey)
	v := s.ReqID
	s.ReqID++
	return v
}

func (c *RPCController) storeApprovalLocked(sessionKey string, req ApprovalRequest) ApprovalRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.getSessionLocked(sessionKey)
	s.ApprovalSeq++
	req.ID = fmt.Sprintf("apr-%04d", s.ApprovalSeq)
	req.SessionKey = sessionKey
	if req.Kind == "" {
		req.Kind = ApprovalKindUnknown
	}
	if strings.TrimSpace(req.RequestedAt) == "" {
		req.RequestedAt = time.Now().Format("2006-01-02 15:04:05")
	}
	req.Summary = shortText(strings.TrimSpace(req.Summary), 500)
	req.RawPayload = shortText(strings.TrimSpace(req.RawPayload), 2000)
	s.Approvals[req.ID] = req
	return req
}

func (c *RPCController) tryBuildApproval(sessionKey string, err error, content string) (ApprovalRequest, bool) {
	if remote := readRemoteError(err); remote != nil {
		if req, ok := buildApprovalFromRemoteError(sessionKey, remote); ok {
			return c.storeApprovalLocked(sessionKey, req), true
		}
	}
	if req, ok := buildApprovalFromEnvelope(sessionKey, err); ok {
		return c.storeApprovalLocked(sessionKey, req), true
	}
	if req, ok := buildApprovalFromError(sessionKey, err); ok {
		return c.storeApprovalLocked(sessionKey, req), true
	}
	if req, ok := buildApprovalFromText(sessionKey, content); ok {
		return c.storeApprovalLocked(sessionKey, req), true
	}
	return ApprovalRequest{}, false
}

func buildApprovalFromError(sessionKey string, err error) (ApprovalRequest, bool) {
	if err == nil {
		return ApprovalRequest{}, false
	}
	if req, ok := buildApprovalFromText(sessionKey, err.Error()); ok {
		return req, true
	}
	remote := readRemoteError(err)
	if remote == nil {
		return ApprovalRequest{}, false
	}
	if req, ok := buildApprovalFromText(sessionKey, remote.Message); ok {
		return req, true
	}
	return buildApprovalFromText(sessionKey, fmt.Sprintf("%v", remote.Data))
}

func buildApprovalFromRemoteError(sessionKey string, remote *JSONRPCRemoteError) (ApprovalRequest, bool) {
	if remote == nil {
		return ApprovalRequest{}, false
	}
	if dataMap, ok := remote.Data.(map[string]interface{}); ok {
		if req, ok := buildApprovalFromStructuredData(sessionKey, dataMap); ok {
			return req, true
		}
	}
	if req, ok := buildApprovalFromText(sessionKey, remote.Message); ok {
		return req, true
	}
	return buildApprovalFromText(sessionKey, fmt.Sprintf("%v", remote.Data))
}

func buildApprovalFromEnvelope(sessionKey string, err error) (ApprovalRequest, bool) {
	if err == nil {
		return ApprovalRequest{}, false
	}
	raw := strings.TrimSpace(err.Error())
	if raw == "" {
		return ApprovalRequest{}, false
	}
	var msg map[string]interface{}
	if json.Unmarshal([]byte(raw), &msg) != nil {
		i := strings.Index(raw, "{")
		j := strings.LastIndex(raw, "}")
		if i < 0 || j <= i {
			return ApprovalRequest{}, false
		}
		if json.Unmarshal([]byte(raw[i:j+1]), &msg) != nil {
			return ApprovalRequest{}, false
		}
	}
	if req, ok := buildApprovalFromStructuredData(sessionKey, msg); ok {
		return req, true
	}
	return ApprovalRequest{}, false
}

func buildApprovalFromStructuredData(sessionKey string, data map[string]interface{}) (ApprovalRequest, bool) {
	if data == nil {
		return ApprovalRequest{}, false
	}
	summary, ok := extractApprovalSummaryFromMap(data)
	if !ok {
		return ApprovalRequest{}, false
	}
	return ApprovalRequest{
		SessionKey:  sessionKey,
		Kind:        ApprovalKindUnknown,
		Summary:     shortText(summary, 500),
		RequestedAt: time.Now().Format("2006-01-02 15:04:05"),
		RawPayload:  shortText(encodeCompactJSONRPC(data), 2000),
	}, true
}

func buildApprovalFromResponse(sessionKey string, resp map[string]interface{}) (ApprovalRequest, bool) {
	if resp == nil {
		return ApprovalRequest{}, false
	}
	if method, _ := resp["method"].(string); strings.Contains(strings.ToLower(method), "requestapproval") {
		params, _ := resp["params"].(map[string]interface{})
		req, ok := buildApprovalFromStructuredData(sessionKey, params)
		if !ok {
			return ApprovalRequest{}, false
		}
		req.Kind = approvalKindFromMethodRPC(method)
		req.RequestID = strings.TrimSpace(fmt.Sprintf("%v", resp["id"]))
		if strings.TrimSpace(req.Summary) == "" {
			req.Summary = buildApprovalSummaryRPC(method, params)
		}
		return req, true
	}
	if result, ok := resp["result"].(map[string]interface{}); ok {
		return buildApprovalFromStructuredData(sessionKey, result)
	}
	return buildApprovalFromStructuredData(sessionKey, resp)
}

func approvalKindFromMethodRPC(method string) ApprovalKind {
	m := strings.ToLower(strings.TrimSpace(method))
	switch {
	case strings.Contains(m, "commandexecution"), strings.Contains(m, "execcommand"):
		return ApprovalKindCommand
	case strings.Contains(m, "filechange"), strings.Contains(m, "applypatch"):
		return ApprovalKindFileChange
	default:
		return ApprovalKindUnknown
	}
}

func buildApprovalSummaryRPC(method string, params map[string]interface{}) string {
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

func buildApprovalFromText(sessionKey, text string) (ApprovalRequest, bool) {
	summary, ok := detectApprovalSignal(text)
	if !ok {
		return ApprovalRequest{}, false
	}
	return ApprovalRequest{
		SessionKey:  sessionKey,
		Kind:        ApprovalKindUnknown,
		Summary:     shortText(summary, 500),
		RequestedAt: time.Now().Format("2006-01-02 15:04:05"),
		RawPayload:  shortText(text, 2000),
	}, true
}

func detectApprovalSignal(text string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return "", false
	}
	keys := []string{
		"permission required",
		"approval required",
		"requires approval",
		"please approve",
		"confirm to continue",
		"requires_approval",
		"approval_request",
		"need approval",
		"승인",
		"허가",
		"권한",
	}
	for _, k := range keys {
		if strings.Contains(t, k) {
			return normalizeApprovalSummary(text), true
		}
	}
	if summary, ok := detectApprovalFromJSON(text); ok {
		return summary, true
	}
	return "", false
}

func normalizeApprovalSummary(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return "approval requested"
	}
	if len([]rune(s)) > 700 {
		return shortText(s, 700)
	}
	return s
}

func detectApprovalFromJSON(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", false
	}
	var obj map[string]interface{}
	if json.Unmarshal([]byte(trimmed), &obj) == nil {
		if summary, ok := extractApprovalSummaryFromMap(obj); ok {
			return summary, true
		}
	}
	i := strings.Index(trimmed, "{")
	j := strings.LastIndex(trimmed, "}")
	if i < 0 || j <= i {
		return "", false
	}
	var nested map[string]interface{}
	if json.Unmarshal([]byte(trimmed[i:j+1]), &nested) == nil {
		if summary, ok := extractApprovalSummaryFromMap(nested); ok {
			return summary, true
		}
	}
	return "", false
}

func extractApprovalSummaryFromMap(obj map[string]interface{}) (string, bool) {
	if obj == nil {
		return "", false
	}
	boolKeys := []string{"requiresApproval", "requires_approval", "approvalRequired", "approval_required"}
	for _, k := range boolKeys {
		if v, ok := obj[k].(bool); ok && v {
			if summary, ok := readFirstString(obj, "approvalSummary", "approval_summary", "message", "reason", "detail"); ok {
				return normalizeApprovalSummary(summary), true
			}
			return "approval requested", true
		}
	}
	if req, ok := obj["approvalRequest"].(map[string]interface{}); ok && req != nil {
		if summary, ok := readFirstString(req, "summary", "message", "reason", "title"); ok {
			return normalizeApprovalSummary(summary), true
		}
		return "approval requested", true
	}
	if req, ok := obj["approval_request"].(map[string]interface{}); ok && req != nil {
		if summary, ok := readFirstString(req, "summary", "message", "reason", "title"); ok {
			return normalizeApprovalSummary(summary), true
		}
		return "approval requested", true
	}
	for _, nestedKey := range []string{"result", "data", "payload"} {
		if nested, ok := obj[nestedKey].(map[string]interface{}); ok && nested != nil {
			if summary, ok := extractApprovalSummaryFromMap(nested); ok {
				return summary, true
			}
		}
	}
	return "", false
}

func readFirstString(obj map[string]interface{}, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

func shortText(s string, limit int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= limit {
		return string(r)
	}
	return string(r[:limit]) + "..."
}

func encodeCompactJSONRPC(v interface{}) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseJSONLineMap(line string) (map[string]interface{}, bool) {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return nil, false
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		raw = strings.TrimSpace(strings.TrimPrefix(raw, "data:"))
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil {
			if mm, ok := parseJSONLineMap(s); ok {
				return mm, true
			}
		}
		if !strings.Contains(raw, "{") {
			return nil, false
		}
		i := strings.Index(raw, "{")
		j := strings.LastIndex(raw, "}")
		if i < 0 || j <= i {
			return nil, false
		}
		var m2 map[string]interface{}
		if err2 := json.Unmarshal([]byte(strings.TrimSpace(raw[i:j+1])), &m2); err2 != nil {
			return nil, false
		}
		return m2, true
	}
	return m, true
}

func extractRawLineText(line string) string {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return ""
	}
	raw = stripDaemonTag(raw)
	if raw == "" {
		return ""
	}
	if strings.EqualFold(raw, "__eof__") {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		raw = strings.TrimSpace(strings.TrimPrefix(raw, "data:"))
	}
	if raw == "" || strings.EqualFold(raw, "__eof__") {
		return ""
	}
	if _, ok := parseJSONLineMap(raw); ok {
		return ""
	}
	return shortText(sanitizeMCPText(raw), 2000)
}

func stripDaemonTag(raw string) string {
	tags := []string{"[stdout]", "[stderr]", "[pty]"}
	for _, tag := range tags {
		if strings.HasPrefix(raw, tag) {
			return strings.TrimSpace(strings.TrimPrefix(raw, tag))
		}
	}
	return raw
}

func sanitizeMCPText(s string) string {
	s = ansiEscapeRE.ReplaceAllString(s, "")
	s = ansiOrphanSGRRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\u0000", "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

func parseStreamEvents(envelope map[string]interface{}, requestMethod string) []streamEvent {
	if envelope == nil {
		return nil
	}
	var out []streamEvent
	if remoteErr, ok := parseEnvelopeRemoteError(envelope, requestMethod); ok {
		out = append(out, streamEvent{Kind: streamEventError, RemoteErr: remoteErr})
	}

	method, _ := envelope["method"].(string)
	if method == "codex/event" {
		params, _ := envelope["params"].(map[string]interface{})
		msg, _ := params["msg"].(map[string]interface{})
		out = append(out, parseCodexMessageEvent(msg)...)
		return out
	}

	// Fallback for line formats that emit events directly without jsonrpc wrapper.
	out = append(out, parseCodexMessageEvent(envelope)...)
	return out
}

func parseEnvelopeRemoteError(envelope map[string]interface{}, requestMethod string) (*JSONRPCRemoteError, bool) {
	if envelope == nil {
		return nil, false
	}
	errObj, _ := envelope["error"].(map[string]interface{})
	if errObj == nil {
		return nil, false
	}
	code := 0
	switch v := errObj["code"].(type) {
	case float64:
		code = int(v)
	case int:
		code = v
	}
	message, _ := errObj["message"].(string)
	return &JSONRPCRemoteError{
		Code:    code,
		Message: shortText(strings.TrimSpace(message), 500),
		Data:    errObj["data"],
		Method:  requestMethod,
	}, true
}

func parseCodexMessageEvent(msg map[string]interface{}) []streamEvent {
	if msg == nil {
		return nil
	}
	var out []streamEvent
	typ, _ := msg["type"].(string)
	typ = strings.TrimSpace(typ)

	if typ == "thread.started" {
		if tid, _ := msg["thread_id"].(string); tid != "" {
			out = append(out, streamEvent{Kind: streamEventInit, SessionID: tid})
		}
	}
	// Ignore incremental agent message deltas to avoid leaking analysis/progress chatter.
	// Prefer structured final answer extraction from raw_response_item (phase=final_answer).

	if typ == "turn.completed" {
		usage, _ := msg["usage"].(map[string]interface{})
		out = append(out, streamEvent{Kind: streamEventResult, Usage: usage})
	}
	if typ == "turn.failed" {
		errObj, _ := msg["error"].(map[string]interface{})
		message, _ := errObj["message"].(string)
		out = append(out, streamEvent{Kind: streamEventError, RemoteErr: &JSONRPCRemoteError{Code: -1, Message: shortText(message, 500)}})
	}

	// Legacy message format: {"type":"message","role":"assistant","content":[...]}
	if typ == "message" {
		if role, _ := msg["role"].(string); role == "assistant" {
			out = append(out, extractAssistantTextEvents(msg)...)
		}
	}

	// MCP wrapper format used by codex/event
	if typ == "raw_response_item" {
		item, _ := msg["item"].(map[string]interface{})
		if item != nil {
			out = append(out, parseCodexItem(item)...)
		}
	}

	// Some emitters place item/type directly
	if strings.HasPrefix(typ, "item.") || strings.HasPrefix(typ, "item_") {
		item, _ := msg["item"].(map[string]interface{})
		if item != nil {
			if t, _ := item["type"].(string); t == "reasoning" {
				out = append(out, streamEvent{Kind: streamEventStatus, Status: "thinking"})
			}
			if typ == "item.started" || typ == "item_started" {
				if tool := detectToolName(item); tool != "" {
					out = append(out, streamEvent{Kind: streamEventTool, ToolName: tool})
				}
			}
			if t, _ := item["type"].(string); t == "agent_message" && (typ == "item.completed" || typ == "item_completed") {
				if txt, _ := item["text"].(string); strings.TrimSpace(txt) != "" {
					out = append(out, streamEvent{Kind: streamEventText, Text: txt})
				}
			}
		}
	}

	// Fallback raw extraction for ambiguous shapes.
	out = append(out, extractAssistantTextEvents(msg)...)
	if tid := extractThreadIDFromAny(msg); tid != "" {
		out = append(out, streamEvent{Kind: streamEventInit, SessionID: tid})
	}
	return compactStreamEvents(out)
}

func parseCodexItem(item map[string]interface{}) []streamEvent {
	if item == nil {
		return nil
	}
	var out []streamEvent
	if role, _ := item["role"].(string); role == "assistant" {
		phase, _ := item["phase"].(string)
		phase = strings.TrimSpace(strings.ToLower(phase))
		if phase == "" || phase == "final_answer" {
			out = append(out, extractAssistantTextEvents(item)...)
		}
	}
	if txt, _ := item["text"].(string); strings.TrimSpace(txt) != "" {
		t, _ := item["type"].(string)
		if t == "agent_message" || t == "output_text" || t == "text" {
			out = append(out, streamEvent{Kind: streamEventText, Text: txt})
		}
	}
	if t, _ := item["type"].(string); t == "reasoning" {
		out = append(out, streamEvent{Kind: streamEventStatus, Status: "thinking"})
	}
	if tool := detectToolName(item); tool != "" {
		out = append(out, streamEvent{Kind: streamEventTool, ToolName: tool})
	}
	if tid := extractThreadIDFromAny(item); tid != "" {
		out = append(out, streamEvent{Kind: streamEventInit, SessionID: tid})
	}
	return compactStreamEvents(out)
}

func extractAssistantTextEvents(obj map[string]interface{}) []streamEvent {
	content, _ := obj["content"].([]interface{})
	if len(content) == 0 {
		return nil
	}
	var out []streamEvent
	for _, c := range content {
		part, _ := c.(map[string]interface{})
		if part == nil {
			continue
		}
		t, _ := part["type"].(string)
		if t != "output_text" && t != "text" && t != "input_text" {
			continue
		}
		txt, _ := part["text"].(string)
		if strings.TrimSpace(txt) != "" {
			out = append(out, streamEvent{Kind: streamEventText, Text: txt})
		}
	}
	return out
}

func detectToolName(item map[string]interface{}) string {
	if item == nil {
		return ""
	}
	if name, _ := item["tool_name"].(string); name != "" {
		return name
	}
	if name, _ := item["name"].(string); name != "" {
		return name
	}
	t, _ := item["type"].(string)
	switch t {
	case "mcp_tool_call":
		return "MCP"
	case "command_execution":
		return "Bash"
	case "file_change":
		return "Edit"
	case "web_search":
		return "WebSearch"
	default:
		return ""
	}
}

func extractThreadIDFromAny(obj map[string]interface{}) string {
	if obj == nil {
		return ""
	}
	keys := []string{"threadId", "thread_id", "session_id"}
	for _, k := range keys {
		if v, _ := obj[k].(string); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	meta, _ := obj["_meta"].(map[string]interface{})
	if meta != nil {
		if v, _ := meta["threadId"].(string); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func compactStreamEvents(in []streamEvent) []streamEvent {
	if len(in) == 0 {
		return nil
	}
	out := make([]streamEvent, 0, len(in))
	seenText := map[string]struct{}{}
	for _, ev := range in {
		if ev.Kind == streamEventText {
			k := strings.TrimSpace(ev.Text)
			if k == "" {
				continue
			}
			if _, ok := seenText[k]; ok {
				continue
			}
			seenText[k] = struct{}{}
		}
		out = append(out, ev)
	}
	return out
}

func extractMCPToolResult(resp map[string]interface{}) (threadID, content string) {
	result, _ := resp["result"].(map[string]interface{})
	if result == nil {
		return "", ""
	}
	if c, ok := result["content"].([]interface{}); ok && len(c) > 0 {
		var parts []string
		for _, it := range c {
			obj, _ := it.(map[string]interface{})
			if obj == nil {
				continue
			}
			t, _ := obj["type"].(string)
			if t == "text" || t == "output_text" || t == "input_text" {
				if txt, _ := obj["text"].(string); txt != "" {
					var embedded map[string]interface{}
					if json.Unmarshal([]byte(txt), &embedded) == nil {
						if tid := extractThreadIDFromAny(embedded); tid != "" {
							threadID = tid
						}
						if cc, _ := embedded["content"].(string); cc != "" {
							parts = append(parts, cc)
							continue
						}
					}
					parts = append(parts, txt)
				}
			}
		}
		if len(parts) > 0 {
			content = strings.TrimSpace(strings.Join(parts, "\n"))
		}
	}
	if tid, _ := result["threadId"].(string); tid != "" {
		threadID = tid
	}
	if tid := extractThreadIDFromAny(result); tid != "" {
		threadID = tid
	}
	if cc, _ := result["content"].(string); cc != "" {
		content = cc
	}
	return
}

func extractThreadIDFromEvent(m map[string]interface{}) string {
	params, _ := m["params"].(map[string]interface{})
	if params == nil {
		return ""
	}
	meta, _ := params["_meta"].(map[string]interface{})
	if meta == nil {
		return ""
	}
	tid, _ := meta["threadId"].(string)
	return tid
}

func extractAssistantTextFromEvent(m map[string]interface{}) string {
	if method, _ := m["method"].(string); method != "codex/event" {
		return ""
	}
	params, _ := m["params"].(map[string]interface{})
	if params == nil {
		return ""
	}
	msg, _ := params["msg"].(map[string]interface{})
	if msg == nil {
		return ""
	}
	if typ, _ := msg["type"].(string); typ != "raw_response_item" {
		return ""
	}
	item, _ := msg["item"].(map[string]interface{})
	if item == nil {
		return ""
	}
	if role, _ := item["role"].(string); role != "assistant" {
		return ""
	}
	content, _ := item["content"].([]interface{})
	for _, c := range content {
		obj, _ := c.(map[string]interface{})
		if obj == nil {
			continue
		}
		if t, _ := obj["type"].(string); t == "output_text" || t == "text" {
			if txt, _ := obj["text"].(string); txt != "" {
				return txt
			}
		}
	}
	return ""
}

func (c *RPCController) call(ctx context.Context, method string, req any, out any) error {
	c.mu.Lock()
	addr := strings.TrimSpace(c.Addr)
	c.mu.Unlock()
	if addr == "" {
		addr = defaultCodexDaemonAddr
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("rpc dial %s: %w", addr, err)
	}
	client := rpc.NewClient(conn)
	defer client.Close()
	call := client.Go(method, req, out, make(chan *rpc.Call, 1))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case done := <-call.Done:
		if done.Error != nil {
			return done.Error
		}
		return nil
	}
}

func (c *RPCController) CleanupSessions(sessionTTL time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		return
	}
	c.store.CleanupExpired(sessionTTL)
}

func codexDaemonAddrFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("MANTISX_CODEX_DAEMON_ADDR")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("BACKTESTER_CODEX_DAEMON_ADDR")); v != "" {
		return v
	}
	return defaultCodexDaemonAddr
}

func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func isRPCMethodNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "can't find method") || strings.Contains(msg, "method not found")
}

func isNoJSONLineError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no json response line")
}

func isInitDeadlineExceeded(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "mcp initialize") && strings.Contains(msg, "deadline exceeded")
}

func isInitializeAlreadyCalled(err error) bool {
	if err == nil {
		return false
	}
	var remoteErr *JSONRPCRemoteError
	if errors.As(err, &remoteErr) && remoteErr != nil {
		msg := strings.ToLower(remoteErr.Message)
		if strings.Contains(msg, "initialize called more than once") {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "initialize called more than once")
}
