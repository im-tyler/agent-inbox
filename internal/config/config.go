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
	King struct {
		// Rounds is how many dispatch rounds one king turn may spend before
		// it must report back. 0 or 1 means the king dispatches once, reads
		// the replies and answers — the default. Higher lets it act on what a
		// reply revealed (ask B about what A just said) at the cost of that
		// many more agent turns per message, unattended. Clamped internally.
		Rounds int `json:"rounds"`
		// Name is what the supervisor is called. Defaults to "supervisor".
		Name string `json:"name"`
		// Tool is the driver it runs on. Defaults to "claude".
		Tool string `json:"tool"`
		// Dir is the folder its session lives in. Defaults to a "supervisor"
		// directory beside config.json, created on first run.
		//
		// The supervisor gets a folder of its own rather than borrowing a
		// project's for a reason that is not tidiness: an agent session is
		// anchored to a working directory, and pointing it at one of the
		// repos it supervises gives it file access to that repo, excludes
		// that repo from its own fleet, and interleaves supervision with
		// whatever else that project is doing.
		Dir string `json:"dir"`
	} `json:"king"`
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
	return &s, nil
}

// Validate checks that the settings are usable.
//
// Zero projects is no longer an error. The supervisor is provisioned rather
// than configured, so an empty list means a supervisor and nothing to
// supervise — a usable state you can add to with `n` from inside the TUI.
// Refusing to start sent a first-run user to a text editor instead.
func Validate(s *Settings) error {
	for i, p := range s.Projects {
		if p.Name == "" || p.Dir == "" {
			return fmt.Errorf("project %d: name and dir are required", i+1)
		}
	}
	return nil
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

// RemoveProject removes the project with the given name. Returns true if
// found and removed, false if no project matched.
func (s *Settings) RemoveProject(name string) bool {
	for i, existing := range s.Projects {
		if existing.Name == name {
			s.Projects = append(s.Projects[:i], s.Projects[i+1:]...)
			return true
		}
	}
	return false
}

// SetProjectTool updates the Tool field of the project with the given name.
// Returns true if found and updated, false if no project matched.
func (s *Settings) SetProjectTool(name, tool string) bool {
	for i := range s.Projects {
		if s.Projects[i].Name == name {
			s.Projects[i].Tool = tool
			return true
		}
	}
	return false
}

// KnownTools is the canonical list of driver names the UI can offer.
var KnownTools = []string{"claude", "opencode", "codex", "mock"}
