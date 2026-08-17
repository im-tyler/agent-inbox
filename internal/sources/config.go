package sources

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// SourceConfig is one configured producer. Exactly one of Command or URL is
// set; Kind "claude" needs neither.
type SourceConfig struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind,omitempty"` // claude | opencode | codex | exec | http (inferred when empty)
	Command  []string `json:"command,omitempty"`
	Dir      string   `json:"dir,omitempty"`
	URL      string   `json:"http,omitempty"`
	TokenEnv string   `json:"token_env,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`

	// Bin overrides the source's executable (claude, opencode, …).
	Bin string `json:"bin,omitempty"`
	// Claude: where transcripts live. Codex: the sessions root.
	Root string `json:"root,omitempty"`
	// OpenCode-only: path to opencode's SQLite database.
	OpenCodeDB string `json:"opencode_db,omitempty"`
}

type Config struct {
	Sources []SourceConfig `json:"sources"`
}

// ConfigPath is where the source list lives. AGENT_INBOX_SOURCES overrides it.
func ConfigPath() string {
	if p := os.Getenv("AGENT_INBOX_SOURCES"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "sources.json"
	}
	return filepath.Join(home, ".config", "agent-inbox", "sources.json")
}

// Load reads the config, or synthesizes a default one when the file is absent.
//
// An existing file that decodes to zero sources now stays at zero. Treating it
// as "use the defaults" meant there was no way to say "discover nothing" —
// `{"sources": []}` silently turned everything back on.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Default discovers the agent CLIs that are actually installed.
//
// Every supported tool is probed the same way. Claude used to be added
// unconditionally while Codex was not probed at all, so a Codex user saw none
// of their sessions and a machine without Claude carried a source that could
// only fail — neither matching the documented promise to pick up whichever
// supported CLIs are present.
func Default() Config {
	var cfg Config
	for _, t := range []struct{ name, kind, bin string }{
		{"claude-code", "claude", "claude"},
		{"opencode", "opencode", "opencode"},
		{"codex", "codex", "codex"},
	} {
		if _, err := exec.LookPath(t.bin); err == nil {
			cfg.Sources = append(cfg.Sources, SourceConfig{Name: t.name, Kind: t.kind})
		}
	}
	if path, err := exec.LookPath("teploy-ship"); err == nil {
		cfg.Sources = append(cfg.Sources, SourceConfig{
			Name:    "teploy-ship",
			Kind:    "exec",
			Command: []string{path, "inbox", "--json"},
		})
	}
	return cfg
}

// Build turns config into live sources, discarding the diagnostics. Callers
// that can show a problem to the user should prefer BuildWithDiagnostics.
func (c Config) Build() []Source {
	out, _ := c.BuildWithDiagnostics()
	return out
}

// BuildWithDiagnostics turns config into live sources and reports what it
// could not use.
//
// A malformed entry is still skipped rather than fatal — one typo should not
// cost you the whole inbox — but it is no longer skipped silently. An
// unreadable source and an empty one produced identical output: a misspelled
// kind ("opencdoe") simply vanished, and the user saw an empty list and
// reasonably concluded nothing was waiting.
func (c Config) BuildWithDiagnostics() ([]Source, []error) {
	out := make([]Source, 0, len(c.Sources))
	var problems []error
	seen := map[string]bool{}

	for i, s := range c.Sources {
		if s.Disabled {
			continue
		}
		if s.Name == "" {
			problems = append(problems, fmt.Errorf("source %d: name is required", i+1))
			continue
		}
		if seen[s.Name] {
			// Origin is part of an item's identity, so two sources sharing a
			// name make their items collide in the merge.
			problems = append(problems, fmt.Errorf("source %q: duplicate name", s.Name))
			continue
		}
		seen[s.Name] = true

		kind := s.Kind
		if kind == "" {
			switch {
			case len(s.Command) > 0:
				kind = "exec"
			case s.URL != "":
				kind = "http"
			}
		}
		if len(s.Command) > 0 && s.URL != "" {
			problems = append(problems, fmt.Errorf("source %q: has both command and http; pick one", s.Name))
			continue
		}
		switch kind {
		case "claude":
			out = append(out, Claude{Label: s.Name, Root: s.Root, Bin: s.Bin})
		case "opencode":
			out = append(out, OpenCode{Label: s.Name, Bin: s.Bin, DB: s.OpenCodeDB})
		case "codex":
			out = append(out, Codex{Label: s.Name, Bin: s.Bin, Root: s.Root})
		case "exec":
			if len(s.Command) == 0 {
				problems = append(problems, fmt.Errorf("source %q: kind exec needs a command", s.Name))
				continue
			}
			out = append(out, Exec{Label: s.Name, Command: s.Command, Dir: s.Dir})
		case "http":
			if s.URL == "" {
				problems = append(problems, fmt.Errorf("source %q: kind http needs a url", s.Name))
				continue
			}
			out = append(out, HTTP{Label: s.Name, URL: s.URL, TokenEnv: s.TokenEnv})
		case "":
			problems = append(problems, fmt.Errorf("source %q: no kind, and neither command nor http to infer one from", s.Name))
		default:
			problems = append(problems, fmt.Errorf("source %q: unknown kind %q (known: claude, opencode, codex, exec, http)", s.Name, kind))
		}
	}
	return out, problems
}
