package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/im-tyler/agent-inbox/internal/fsutil"
	"github.com/im-tyler/agent-inbox/internal/ident"
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
	// TurnTimeoutSeconds bounds a single agent turn. 0 selects the default;
	// a negative value means no timeout at all.
	//
	// The old hard-coded five minutes was shorter than the work: a coding
	// agent running a build or a test suite legitimately exceeds it, and the
	// turn was killed and reported as a failure with no way to raise the
	// limit. Cancellation already exists for turns that genuinely hang.
	TurnTimeoutSeconds int `json:"turn_timeout_seconds"`

	Projects []Project `json:"projects"`
}

// DefaultTurnTimeout bounds one agent turn unless config says otherwise.
const DefaultTurnTimeout = 30 * time.Minute

// TurnTimeout resolves the configured per-turn deadline. A non-positive
// configured value means no deadline, expressed as zero.
func (s *Settings) TurnTimeout() time.Duration {
	switch {
	case s.TurnTimeoutSeconds == 0:
		return DefaultTurnTimeout
	case s.TurnTimeoutSeconds < 0:
		return 0
	default:
		return time.Duration(s.TurnTimeoutSeconds) * time.Second
	}
}

type Project struct {
	Name string `json:"name"`
	Tool string `json:"tool"` // "claude" | "opencode" | "codex" | "mock"
	Dir  string `json:"dir"`
}

// Load reads settings from path.
//
// A missing file is not an error: it means a first run, and the answer is the
// defaults. Startup used to exit here, which contradicted the documented
// zero-config first run — a fresh install was sent to a text editor to write
// JSON before it could show anything.
//
// A file that exists but does not parse is still an error. Silently treating a
// typo as "no settings" would start the program with none of the user's
// projects and no indication why.
func Load(path string) (*Settings, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Settings{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &s, nil
}

// Exists reports whether a config file is present at path. Callers that need
// to distinguish "first run" from "configured" need this, because Load
// deliberately reports both as a valid Settings.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Validate checks that the settings are usable.
//
// Zero projects is no longer an error. The supervisor is provisioned rather
// than configured, so an empty list means a supervisor and nothing to
// supervise — a usable state you can add to with `n` from inside the TUI.
// Refusing to start sent a first-run user to a text editor instead.
//
// What is checked here is everything that later becomes CLI arguments or
// session identity. A bad value caught at startup names the field; the same
// value caught at send time surfaces as an opaque failure from a subprocess.
func Validate(s *Settings) error {
	kingName := s.King.Name
	if kingName == "" {
		kingName = DefaultKingName
	}
	if err := ident.ValidateName(kingName); err != nil {
		return fmt.Errorf("king.name: %w", err)
	}
	if s.King.Tool != "" && !IsKnownTool(s.King.Tool) {
		return fmt.Errorf("king.tool: unknown tool %q (known: %s)", s.King.Tool, strings.Join(KnownTools, ", "))
	}
	if s.King.Rounds < 0 {
		return fmt.Errorf("king.rounds: must not be negative, got %d", s.King.Rounds)
	}
	if s.Claude.PermissionMode != "" && !contains(ClaudePermissionModes, s.Claude.PermissionMode) {
		return fmt.Errorf("claude.permission_mode: unknown mode %q (known: %s)",
			s.Claude.PermissionMode, strings.Join(ClaudePermissionModes, ", "))
	}
	if s.Codex.Sandbox != "" && !contains(CodexSandboxes, s.Codex.Sandbox) {
		return fmt.Errorf("codex.sandbox: unknown sandbox %q (known: %s)",
			s.Codex.Sandbox, strings.Join(CodexSandboxes, ", "))
	}

	names := make(map[string]int, len(s.Projects))
	dirs := make(map[string]int, len(s.Projects))
	for i, p := range s.Projects {
		if p.Name == "" || p.Dir == "" {
			return fmt.Errorf("project %d: name and dir are required", i+1)
		}
		if err := ident.ValidateName(p.Name); err != nil {
			return fmt.Errorf("project %d: %w", i+1, err)
		}
		if ident.SameName(p.Name, kingName) {
			return fmt.Errorf("project %d: %q is reserved for the supervisor — rename the project or set king.name",
				i+1, p.Name)
		}
		if p.Tool != "" && !IsKnownTool(p.Tool) {
			return fmt.Errorf("project %d (%s): unknown tool %q (known: %s)",
				i+1, p.Name, p.Tool, strings.Join(KnownTools, ", "))
		}
		if prev, dup := names[ident.Name(p.Name)]; dup {
			return fmt.Errorf("project %d (%s): duplicate name, already used by project %d",
				i+1, p.Name, prev+1)
		}
		names[ident.Name(p.Name)] = i
		key := ident.Dir(p.Dir)
		if prev, dup := dirs[key]; dup {
			return fmt.Errorf("project %d (%s): directory %s is already used by project %d",
				i+1, p.Name, p.Dir, prev+1)
		}
		dirs[key] = i
	}
	if s.King.Dir != "" {
		if prev, clash := dirs[ident.Dir(s.King.Dir)]; clash {
			return fmt.Errorf("king.dir collides with project %d (%s) — the supervisor needs a directory of its own",
				prev+1, s.Projects[prev].Name)
		}
	}
	return nil
}

func contains(list []string, s string) bool { return slices.Contains(list, s) }

// Save writes the settings back to path atomically. Used by the TUI when
// adding a project at runtime so new projects persist across restarts
// alongside the originally-configured ones.
func Save(path string, s *Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return fsutil.WriteFileAtomic(path, b, fsutil.FileMode)
}

// AddProject appends a project to s.Projects if (name, dir) isn't already
// present. Returns true if added, false if a duplicate was skipped.
func (s *Settings) AddProject(p Project) bool {
	for _, existing := range s.Projects {
		if ident.SameName(existing.Name, p.Name) || ident.SameDir(existing.Dir, p.Dir) {
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
		if ident.SameName(existing.Name, name) {
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
		if ident.SameName(s.Projects[i].Name, name) {
			s.Projects[i].Tool = tool
			return true
		}
	}
	return false
}

// DefaultKingName is the supervisor's name when config does not set one.
// It lives here rather than in package main because Validate has to reserve
// it, and reserving a name main owns would mean config importing main.
const DefaultKingName = "supervisor"

// KnownTools is the canonical list of driver names the UI can offer.
//
// mock is deliberately absent. It answers instantly with a canned reply, which
// is exactly what a development driver should do and exactly what a user must
// never select by accident: its replies look like real work and no agent ran.
// It stays reachable through AGENT_INBOX_DEV=1 for the tests and for working on
// the TUI without burning tokens.
var KnownTools = []string{"claude", "opencode", "codex"}

// devTool is offered only when the dev flag is set.
const devTool = "mock"

// DevMode reports whether development-only affordances are enabled.
func DevMode() bool { return os.Getenv("AGENT_INBOX_DEV") != "" }

// SelectableTools is what the UI offers in the tool picker and new-project
// modal.
func SelectableTools() []string {
	if DevMode() {
		return append(append([]string{}, KnownTools...), devTool)
	}
	return KnownTools
}

// IsKnownTool reports whether a configured tool name has a driver behind it.
// mock is accepted when explicitly configured even outside dev mode: refusing
// to start on a config that previously worked would be a worse failure than
// the one it prevents.
func IsKnownTool(tool string) bool {
	return contains(KnownTools, tool) || tool == devTool
}

// ClaudePermissionModes are the values Claude Code accepts for
// --permission-mode.
var ClaudePermissionModes = []string{"default", "acceptEdits", "bypassPermissions", "plan"}

// CodexSandboxes are the values Codex accepts for --sandbox.
var CodexSandboxes = []string{"read-only", "workspace-write", "danger-full-access"}
