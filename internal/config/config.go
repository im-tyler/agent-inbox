package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		Model         string `json:"model"`
		Sandbox       string `json:"sandbox"`
		SkipApprovals bool   `json:"skip_approvals"`
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

// Save writes the settings back to path atomically (write to temp + rename).
// Used by the TUI when adding a project at runtime so new projects persist
// across restarts alongside the originally-configured ones.
func Save(path string, s *Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write temp: %w", err)
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// AddProject appends a project to s.Projects if (name, dir) isn't already
// present. Returns true if added, false if a duplicate was skipped.
func (s *Settings) AddProject(p Project) bool {
	for _, existing := range s.Projects {
		if existing.Name == p.Name || existing.Dir == p.Dir {
			return false
		}
	}
	s.Projects = append(s.Projects, p)
	return true
}

// KnownTools is the canonical list of driver names the UI can offer.
var KnownTools = []string{"claude", "opencode", "codex", "mock"}
