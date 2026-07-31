package sources

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// SourceConfig is one configured producer. Exactly one of Command or URL is
// set; Kind "claude" needs neither.
type SourceConfig struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind,omitempty"` // "claude" | "exec" | "http" (inferred when empty)
	Command  []string `json:"command,omitempty"`
	Dir      string   `json:"dir,omitempty"`
	URL      string   `json:"http,omitempty"`
	TokenEnv string   `json:"token_env,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`

	// Claude-only knobs. Root is where transcripts live (for branch/prompt
	// enrichment); Bin overrides the claude executable.
	Root string `json:"root,omitempty"`
	Bin  string `json:"bin,omitempty"`
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
// The default is deliberately useful with zero setup: local Claude Code
// sessions, plus teploy-ship if it happens to be installed. Nobody should have
// to write JSON before seeing whether the tool is worth having.
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
	if len(cfg.Sources) == 0 {
		return Default(), nil
	}
	return cfg, nil
}

func Default() Config {
	cfg := Config{Sources: []SourceConfig{{Name: "claude-code", Kind: "claude"}}}
	if path, err := exec.LookPath("teploy-ship"); err == nil {
		cfg.Sources = append(cfg.Sources, SourceConfig{
			Name:    "teploy-ship",
			Kind:    "exec",
			Command: []string{path, "inbox", "--json"},
		})
	}
	return cfg
}

// Build turns config into live sources, skipping entries that are disabled or
// too malformed to act on. A bad entry is skipped rather than fatal: one typo
// should not cost you the whole inbox.
func (c Config) Build() []Source {
	out := make([]Source, 0, len(c.Sources))
	for _, s := range c.Sources {
		if s.Disabled || s.Name == "" {
			continue
		}
		kind := s.Kind
		if kind == "" {
			switch {
			case len(s.Command) > 0:
				kind = "exec"
			case s.URL != "":
				kind = "http"
			}
		}
		switch kind {
		case "claude":
			out = append(out, Claude{Root: s.Root, Bin: s.Bin})
		case "exec":
			if len(s.Command) == 0 {
				continue
			}
			out = append(out, Exec{Label: s.Name, Command: s.Command, Dir: s.Dir})
		case "http":
			if s.URL == "" {
				continue
			}
			out = append(out, HTTP{Label: s.Name, URL: s.URL, TokenEnv: s.TokenEnv})
		}
	}
	return out
}
