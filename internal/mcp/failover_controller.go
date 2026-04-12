package mcp

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FailoverController routes requests to primary backend and temporarily
// fails over to secondary backend when primary repeatedly fails.
type FailoverController struct {
	primary   Controller
	secondary Controller

	mu sync.Mutex

	primaryFailedUntil time.Time
	cooldown           time.Duration
}

func NewFailoverController(primary, secondary Controller, cooldown time.Duration) *FailoverController {
	if cooldown <= 0 {
		cooldown = 5 * time.Minute
	}
	return &FailoverController{
		primary:   primary,
		secondary: secondary,
		cooldown:  cooldown,
	}
}

func NewFailoverControllerFromEnv(primary Controller) Controller {
	enabled := envBool("MANTISX_FAILOVER_ENABLED", false)
	if !enabled {
		return primary
	}

	mode := strings.ToLower(strings.TrimSpace(os.Getenv("MANTISX_FAILOVER_SECONDARY_MODE")))
	var secondary Controller
	switch mode {
	case "", "rpc":
		secondary = NewRPCControllerFromEnv()
	case "appserver", "app-server", "app_server":
		secondary = NewAppServerControllerFromEnv()
	default:
		secondary = NewRPCControllerFromEnv()
	}

	sec := 300
	if raw := strings.TrimSpace(os.Getenv("MANTISX_FAILOVER_COOLDOWN_SEC")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			sec = n
		}
	}

	return NewFailoverController(primary, secondary, time.Duration(sec)*time.Second)
}

func (c *FailoverController) activePrimaryLocked() bool {
	return time.Now().After(c.primaryFailedUntil)
}

func (c *FailoverController) markPrimaryFailedLocked() {
	c.primaryFailedUntil = time.Now().Add(c.cooldown)
}

func (c *FailoverController) shouldFailover(err error) bool {
	if err == nil {
		return false
	}

	s := strings.ToLower(strings.TrimSpace(err.Error()))
	if s == "" {
		return false
	}

	keys := []string{
		"context deadline exceeded",
		"no json response line",
		"connection closed",
		"broken pipe",
		"initialize",
		"app-server",
		"rpc unavailable",
	}
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func (c *FailoverController) callWithFailover(
	ctx context.Context,
	fnPrimary func(context.Context) (string, error),
	fnSecondary func(context.Context) (string, error),
) (string, error) {
	c.mu.Lock()
	usePrimary := c.activePrimaryLocked()
	c.mu.Unlock()

	if usePrimary {
		out, err := fnPrimary(ctx)
		if err == nil {
			return out, nil
		}
		if !c.shouldFailover(err) {
			return "", err
		}

		c.mu.Lock()
		c.markPrimaryFailedLocked()
		c.mu.Unlock()

		out2, err2 := fnSecondary(ctx)
		if err2 != nil {
			return "", fmt.Errorf("primary failed: %v; secondary failed: %w", err, err2)
		}
		return out2, nil
	}

	out, err := fnSecondary(ctx)
	if err == nil {
		return out, nil
	}
	return "", err
}

func (c *FailoverController) ClearSession(ctx context.Context) error {
	_, err := c.callWithFailover(ctx,
		func(ctx context.Context) (string, error) {
			return "", c.primary.ClearSession(ctx)
		},
		func(ctx context.Context) (string, error) {
			return "", c.secondary.ClearSession(ctx)
		},
	)
	return err
}

func (c *FailoverController) StopDaemon(ctx context.Context) error {
	_, err := c.callWithFailover(ctx,
		func(ctx context.Context) (string, error) {
			return "", c.primary.StopDaemon(ctx)
		},
		func(ctx context.Context) (string, error) {
			return "", c.secondary.StopDaemon(ctx)
		},
	)
	return err
}

func (c *FailoverController) SubmitPrompt(ctx context.Context, prompt string) (string, error) {
	return c.callWithFailover(ctx,
		func(ctx context.Context) (string, error) {
			return c.primary.SubmitPrompt(ctx, prompt)
		},
		func(ctx context.Context) (string, error) {
			return c.secondary.SubmitPrompt(ctx, prompt)
		},
	)
}

func (c *FailoverController) ListApprovals(ctx context.Context) ([]ApprovalRequest, error) {
	// Approval state can exist in either backend during/after failover.
	// Merge both lists to reduce "approval not found" surprises.
	// NOTE(fixlist#F002): this merge is in-memory and non-transactional across backends.
	// If both backends emit similar IDs independently, operator must resolve by summary/context.
	primaryList, primaryErr := c.primary.ListApprovals(ctx)
	secondaryList, secondaryErr := c.secondary.ListApprovals(ctx)
	if primaryErr != nil && secondaryErr != nil {
		// Preserve previous failover behavior when both failed.
		if c.shouldFailover(primaryErr) {
			c.mu.Lock()
			c.markPrimaryFailedLocked()
			c.mu.Unlock()
		}
		return nil, fmt.Errorf("primary approvals failed: %v; secondary approvals failed: %w", primaryErr, secondaryErr)
	}
	seen := map[string]struct{}{}
	merged := make([]ApprovalRequest, 0, len(primaryList)+len(secondaryList))
	for _, ap := range primaryList {
		if _, ok := seen[ap.ID]; ok {
			continue
		}
		seen[ap.ID] = struct{}{}
		merged = append(merged, ap)
	}
	for _, ap := range secondaryList {
		if _, ok := seen[ap.ID]; ok {
			continue
		}
		seen[ap.ID] = struct{}{}
		merged = append(merged, ap)
	}
	return merged, nil
}

func (c *FailoverController) ResolveApprovalDecision(ctx context.Context, id string, decision ApprovalDecision) (string, error) {
	if !decision.Valid() {
		return "", fmt.Errorf("invalid approval decision: %q", decision)
	}
	// Approval can belong to either backend; try both before giving up.
	out, err := c.primary.ResolveApprovalDecision(ctx, id, decision)
	if err == nil {
		return out, nil
	}
	out2, err2 := c.secondary.ResolveApprovalDecision(ctx, id, decision)
	if err2 == nil {
		if c.shouldFailover(err) {
			c.mu.Lock()
			c.markPrimaryFailedLocked()
			c.mu.Unlock()
		}
		return out2, nil
	}
	return "", fmt.Errorf("primary approval resolve failed: %v; secondary approval resolve failed: %w", err, err2)
}

func (c *FailoverController) ApprovalEnabled(ctx context.Context) bool {
	c.mu.Lock()
	usePrimary := c.activePrimaryLocked()
	c.mu.Unlock()

	if usePrimary {
		return c.primary.ApprovalEnabled(ctx)
	}
	return c.secondary.ApprovalEnabled(ctx)
}

func (c *FailoverController) SetApprovalEnabled(ctx context.Context, enabled bool) (string, error) {
	return c.callWithFailover(ctx,
		func(ctx context.Context) (string, error) {
			return c.primary.SetApprovalEnabled(ctx, enabled)
		},
		func(ctx context.Context) (string, error) {
			return c.secondary.SetApprovalEnabled(ctx, enabled)
		},
	)
}

func (c *FailoverController) SetWorkspaceDir(dir string) {
	if w, ok := c.primary.(WorkspaceSetter); ok {
		w.SetWorkspaceDir(dir)
	}
	if w, ok := c.secondary.(WorkspaceSetter); ok {
		w.SetWorkspaceDir(dir)
	}
}

func (c *FailoverController) GetProgress(ctx context.Context) (ProgressState, error) {
	get := func(ctrl Controller) (ProgressState, error) {
		r, ok := ctrl.(ProgressReporter)
		if !ok {
			return ProgressState{}, fmt.Errorf("controller does not support progress: %T", ctrl)
		}
		return r.GetProgress(ctx)
	}

	c.mu.Lock()
	usePrimary := c.activePrimaryLocked()
	c.mu.Unlock()

	if usePrimary {
		st, err := get(c.primary)
		if err == nil {
			return st, nil
		}
		st2, err2 := get(c.secondary)
		if err2 == nil {
			if c.shouldFailover(err) {
				c.mu.Lock()
				c.markPrimaryFailedLocked()
				c.mu.Unlock()
			}
			return st2, nil
		}
		return ProgressState{Phase: "thinking"}, fmt.Errorf("primary progress failed: %v; secondary progress failed: %w", err, err2)
	}

	st, err := get(c.secondary)
	if err == nil {
		return st, nil
	}
	st2, err2 := get(c.primary)
	if err2 == nil {
		return st2, nil
	}
	return ProgressState{Phase: "thinking"}, fmt.Errorf("secondary progress failed: %v; primary progress failed: %w", err, err2)
}
