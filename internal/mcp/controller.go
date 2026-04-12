package mcp

import (
	"context"
	"fmt"
	"strings"
)

type ApprovalKind string

const (
	ApprovalKindCommand    ApprovalKind = "command"
	ApprovalKindFileChange ApprovalKind = "file_change"
	ApprovalKindUnknown    ApprovalKind = "unknown"
)

type ApprovalRequest struct {
	ID          string       `json:"id"`
	SessionKey  string       `json:"session_key"`
	Kind        ApprovalKind `json:"kind"`
	Summary     string       `json:"summary"`
	Command     string       `json:"command,omitempty"`
	Cwd         string       `json:"cwd,omitempty"`
	Files       []string     `json:"files,omitempty"`
	RequestID   string       `json:"request_id,omitempty"`
	RequestedAt string       `json:"requested_at"`
	RawPayload  string       `json:"raw_payload,omitempty"`
}

type ApprovalDecision string

const (
	ApprovalDecisionAccept           ApprovalDecision = "accept"
	ApprovalDecisionAcceptForSession ApprovalDecision = "accept_for_session"
	ApprovalDecisionDecline          ApprovalDecision = "decline"
	ApprovalDecisionCancel           ApprovalDecision = "cancel"
)

func NormalizeApprovalDecision(v string) ApprovalDecision {
	s := strings.ToLower(strings.TrimSpace(v))
	switch s {
	case "accept", "approve", "approved", "1":
		return ApprovalDecisionAccept
	case "acceptforsession", "accept_for_session", "session", "2":
		return ApprovalDecisionAcceptForSession
	case "cancel", "abort", "4":
		return ApprovalDecisionCancel
	default:
		return ApprovalDecisionDecline
	}
}

func (d ApprovalDecision) Valid() bool {
	switch d {
	case ApprovalDecisionAccept,
		ApprovalDecisionAcceptForSession,
		ApprovalDecisionDecline,
		ApprovalDecisionCancel:
		return true
	default:
		return false
	}
}

func (d ApprovalDecision) String() string {
	return string(d)
}

type Controller interface {
	ClearSession(ctx context.Context) error
	StopDaemon(ctx context.Context) error
	SubmitPrompt(ctx context.Context, prompt string) (string, error)
	ListApprovals(ctx context.Context) ([]ApprovalRequest, error)
	ResolveApprovalDecision(ctx context.Context, id string, decision ApprovalDecision) (string, error)
	ApprovalEnabled(ctx context.Context) bool
	SetApprovalEnabled(ctx context.Context, enabled bool) (string, error)
}

type WorkspaceSetter interface {
	SetWorkspaceDir(dir string)
}

type ProgressState struct {
	Phase string `json:"phase"`
	Task  string `json:"task,omitempty"`
}

type ProgressReporter interface {
	GetProgress(ctx context.Context) (ProgressState, error)
}

type NoopController struct{}

func (NoopController) ClearSession(ctx context.Context) error { return nil }
func (NoopController) StopDaemon(ctx context.Context) error   { return nil }
func (NoopController) SubmitPrompt(ctx context.Context, prompt string) (string, error) {
	return "noop: " + prompt, nil
}
func (NoopController) ListApprovals(ctx context.Context) ([]ApprovalRequest, error) { return nil, nil }
func (NoopController) ResolveApprovalDecision(ctx context.Context, id string, decision ApprovalDecision) (string, error) {
	if !decision.Valid() {
		return "", fmt.Errorf("invalid approval decision: %q", decision)
	}
	switch decision {
	case ApprovalDecisionAccept:
		return "noop approval accepted: " + id, nil
	case ApprovalDecisionAcceptForSession:
		return "noop approval accepted for session: " + id, nil
	case ApprovalDecisionCancel:
		return "noop approval canceled: " + id, nil
	default:
		return "noop approval denied: " + id, nil
	}
}
func (NoopController) ApprovalEnabled(ctx context.Context) bool { return false }
func (NoopController) SetApprovalEnabled(ctx context.Context, enabled bool) (string, error) {
	if enabled {
		return "approval mode enabled", nil
	}
	return "approval mode disabled", nil
}

func ResolveApproval(ctx context.Context, c Controller, id string, approve bool) (string, error) {
	if approve {
		return c.ResolveApprovalDecision(ctx, id, ApprovalDecisionAccept)
	}
	return c.ResolveApprovalDecision(ctx, id, ApprovalDecisionDecline)
}

func ResolveApprovalBool(c Controller, ctx context.Context, id string, approve bool) (string, error) {
	return ResolveApproval(ctx, c, id, approve)
}
