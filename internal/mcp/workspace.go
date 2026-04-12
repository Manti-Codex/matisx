package mcp

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func withWorkspacePrefix(workspaceDir, user string) string {
	ws := strings.TrimSpace(workspaceDir)
	if ws == "" {
		return user
	}
	return "Workspace policy: Use only this workspace as project root: " + ws +
		". Do not inspect unrelated repos.\nUser request: " + user
}

func validateWorkspaceDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("workspace dir is required")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("workspace stat failed: %w", err)
	}
	if !info.IsDir() {
		return errors.New("workspace is not a directory")
	}
	return nil
}
