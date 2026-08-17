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
		// Autonomous lets the supervisor start a turn nobody asked for, when a
		// project in its fleet finishes or gets stuck. Off by default, and the
		// default matters: this is the one thing here that spends money while
		// nobody is watching.
		Autonomous bool `json:"autonomous"`
		// WakesPerHour bounds that. 0 selects the default; the value is
		// clamped. It is separate from Rounds because they bound different
		// things — how often it may start, and how far it may go once started.
		WakesPerHour int `json:"wakes_per_hour"`
		// Constraints are standing rules the supervisor must respect, and
		// Priorities what matters most. Both are injected into every turn.
		//
		// They live here, in the file you own, and not in the supervisor's own
		// note store, because of who is allowed to author policy.
		//
		// The supervisor's replies are shaped by what its projects say, and
		// what its projects say is shaped by the repositories, issues and web
		// pages those agents read. A rule it wrote itself would therefore be a
		// rule an attacker could have written — and unlike an observation, a
		// rule binds every later turn, is never filtered out, and does not age
		// out of the store on its own. So the supervisor may propose one; only
		// you ratify it, and ratifying means it appears here.
		Constraints []string `json:"constraints,omitempty"`
		Priorities  []string `json:"priorities,omitempty"`
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

	// Groups splits the fleet between several supervisors. Empty means one
	// supervisor over everything, which is what this program did before groups
	// existed and remains the default.
	Groups []Group `json:"groups,omitempty"`
}

// Group assigns a subset of the fleet to a supervisor of its own.
//
// A supervisor's context is rebuilt on every turn out of its fleet's status
// lines and every note that mentions one of them. That is what makes
// supervision accurate, and it is also what degrades as the fleet grows: seven
// projects means seven status lines and every note about any of them, on every
// message. Splitting the fleet gives each supervisor a smaller and sharper
// brief, and the two conversations stay about different things.
//
// The supervisor is provisioned rather than configured, exactly as the single
// one is — its name and folder are derived from the group's name. King exists
// for the cases where the derived answer is not wanted.
type Group struct {
	Name     string    `json:"name"`
	Projects []string  `json:"projects"`
	King     GroupKing `json:"king,omitzero"`
}

// GroupKing overrides a group's provisioned supervisor. Empty fields keep the
// derived default.
type GroupKing struct {
	Name string `json:"name,omitempty"`
	Tool string `json:"tool,omitempty"`
	Dir  string `json:"dir,omitempty"`
}

// KingName is the supervisor's name for this group: the override when set,
// otherwise "supervisor-<group>".
func (g Group) KingName() string {
	if g.King.Name != "" {
		return g.King.Name
	}
	return DefaultKingName + "-" + g.Name
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
	// Every name a supervisor will occupy, so a project cannot claim one. With
	// groups configured the singular supervisor is not provisioned at all —
	// each group brings its own — so reserving its name too would forbid a
	// perfectly ordinary project called "supervisor".
	reserved := map[string]string{}
	if len(s.Groups) == 0 {
		reserved[ident.Name(kingName)] = "king.name"
	}
	if err := validateGroups(s, reserved); err != nil {
		return err
	}
	if s.King.Tool != "" && !IsKnownTool(s.King.Tool) {
		return fmt.Errorf("king.tool: unknown tool %q (known: %s)", s.King.Tool, strings.Join(KnownTools, ", "))
	}
	if s.King.Rounds < 0 {
		return fmt.Errorf("king.rounds: must not be negative, got %d", s.King.Rounds)
	}
	if s.King.WakesPerHour < 0 {
		return fmt.Errorf("king.wakes_per_hour: must not be negative, got %d", s.King.WakesPerHour)
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
		if where, taken := reserved[ident.Name(p.Name)]; taken {
			return fmt.Errorf("project %d: %q is reserved for a supervisor (%s) — rename the project or the supervisor",
				i+1, p.Name, where)
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
	for _, g := range s.Groups {
		if g.King.Dir == "" {
			continue
		}
		if prev, clash := dirs[ident.Dir(g.King.Dir)]; clash {
			return fmt.Errorf("group %q: king.dir collides with project %d (%s) — a supervisor needs a directory of its own",
				g.Name, prev+1, s.Projects[prev].Name)
		}
	}
	return validateGroupMembership(s, names)
}

// validateGroups checks group shape and reserves the names their supervisors
// will occupy. It runs before the project loop so that a project claiming a
// supervisor's name is reported against the project, where the user can act
// on it.
func validateGroups(s *Settings, reserved map[string]string) error {
	seen := make(map[string]int, len(s.Groups))
	for i, g := range s.Groups {
		if g.Name == "" {
			return fmt.Errorf("group %d: name is required — it labels the tab and derives the supervisor's name", i+1)
		}
		// The group name becomes part of a supervisor name and a directory, so
		// it has to survive both. Validating it here names the field; letting
		// it through surfaces later as a supervisor that cannot be created.
		if err := ident.ValidateName(g.Name); err != nil {
			return fmt.Errorf("group %d: %w", i+1, err)
		}
		if prev, dup := seen[ident.Name(g.Name)]; dup {
			return fmt.Errorf("group %d (%s): duplicate name, already used by group %d", i+1, g.Name, prev+1)
		}
		seen[ident.Name(g.Name)] = i

		king := g.KingName()
		if err := ident.ValidateName(king); err != nil {
			return fmt.Errorf("group %d (%s): supervisor name %q: %w", i+1, g.Name, king, err)
		}
		if where, dup := reserved[ident.Name(king)]; dup {
			return fmt.Errorf("group %d (%s): supervisor name %q is already used by %s", i+1, g.Name, king, where)
		}
		reserved[ident.Name(king)] = fmt.Sprintf("group %q", g.Name)

		if g.King.Tool != "" && !IsKnownTool(g.King.Tool) {
			return fmt.Errorf("group %d (%s): king.tool: unknown tool %q (known: %s)",
				i+1, g.Name, g.King.Tool, strings.Join(KnownTools, ", "))
		}
	}
	return nil
}

// validateGroupMembership checks that every project a group names exists, and
// that no project is claimed twice.
//
// A project named by no group is not an error. It joins the first group, so
// that adding a project — from config or from the dashboard — can never leave
// it in a fleet no supervisor can see.
func validateGroupMembership(s *Settings, names map[string]int) error {
	if len(s.Groups) == 0 {
		return nil
	}
	claimed := make(map[string]string, len(names))
	for _, g := range s.Groups {
		for _, p := range g.Projects {
			key := ident.Name(p)
			if _, ok := names[key]; !ok {
				return fmt.Errorf("group %q names project %q, which is not in projects", g.Name, p)
			}
			if prev, dup := claimed[key]; dup {
				return fmt.Errorf("project %q is in both group %q and group %q — a project belongs to one supervisor",
					p, prev, g.Name)
			}
			claimed[key] = g.Name
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

// AddKingRule appends a ratified standing rule, skipping duplicates. Returns
// true if it was added.
//
// This is the only way a rule enters the supervisor's policy, and it is only
// reached from an explicit user action in the dashboard.
func (s *Settings) AddKingRule(kind, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	list := &s.King.Constraints
	if kind == "priority" {
		list = &s.King.Priorities
	}
	if slices.Contains(*list, text) {
		return false
	}
	*list = append(*list, text)
	return true
}
