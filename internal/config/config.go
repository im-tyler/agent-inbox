package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Settings struct {
	Claude struct {
		PermissionMode string `json:"permission_mode"`
	} `json:"claude"`
	OpenCode struct {
		Model           string `json:"model"`
		SkipPermissions bool   `json:"skip_permissions"`
	} `json:"opencode"`
	Codex struct {
		Model         string `json:"model"`          // e.g. "gpt-5", "o3"; empty = Codex default
		Sandbox       string `json:"sandbox"`        // "read-only" | "workspace-write" | "danger-full-access"
		SkipApprovals bool   `json:"skip_approvals"` // maps to --dangerously-bypass-approvals-and-sandbox
	} `json:"codex"`
	Projects []Project `json:"projects"`
}

type Project struct {
	Name string `json:"name"`
	Tool string `json:"tool"` // "claude" | "opencode" | "codex" | "mock"
	Dir  string `json:"dir"`
}

func Load(path string) (*Settings, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(s.Projects) == 0 {
		return nil, fmt.Errorf("%s: no projects defined", path)
	}
	return &s, nil
}
