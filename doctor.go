package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/im-tyler/agent-inbox/internal/config"
	"github.com/im-tyler/agent-inbox/internal/fsutil"
	"github.com/im-tyler/agent-inbox/internal/sources"
)

// doctor exists because this program is mostly a consumer of other people's
// command-line interfaces, and those move. Every serious bug this project has
// had in production came from an upstream that changed its flags, its output
// schema or its storage layout, and in each case the symptom was silence: an
// empty inbox, a session that would not resume, a flag rejected only once real
// work was sent. Those failures look identical to "nothing is happening".
//
// So: probe what is installed, say what was found, and separate "no sessions"
// from "cannot read sessions".

type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
)

func (s checkStatus) String() string {
	switch s {
	case statusOK:
		return "OK  "
	case statusWarn:
		return "WARN"
	default:
		return "FAIL"
	}
}

type check struct {
	name   string
	status checkStatus
	detail string
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dd := dataDir()
	cfgPath := fs.String("config", defaultConfigPath(dd), "path to config.json")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var checks []check
	checks = append(checks, toolChecks()...)
	checks = append(checks, helperChecks()...)
	checks = append(checks, pathChecks(dd, *cfgPath)...)
	checks = append(checks, sourceChecks()...)

	worst := statusOK
	for _, c := range checks {
		fmt.Printf("%-22s %s  %s\n", c.name, c.status, c.detail)
		if c.status > worst {
			worst = c.status
		}
	}
	fmt.Println()
	switch worst {
	case statusOK:
		fmt.Println("everything agent-inbox depends on is present.")
	case statusWarn:
		fmt.Println("usable, but something above will limit what the inbox can see.")
	default:
		return fmt.Errorf("a dependency agent-inbox needs is missing or broken")
	}
	return nil
}

// toolChecks probes each agent CLI: present, what version, and whether the
// specific subcommands and flags this program passes still exist.
func toolChecks() []check {
	var out []check
	for _, t := range []struct {
		name        string
		bin         string
		versionArgs []string
		// probe is a help invocation, and want are flags that must appear in
		// it. Checking the flags rather than the version number is deliberate:
		// a version string tells you nothing about whether an interface moved.
		probe []string
		want  []string
	}{
		{
			name: "claude", bin: "claude", versionArgs: []string{"--version"},
			probe: []string{"--help"},
			want:  []string{"--resume", "--print"},
		},
		{
			name: "opencode", bin: "opencode", versionArgs: []string{"--version"},
			probe: []string{"run", "--help"},
			want:  []string{"--session", "--interactive"},
		},
		{
			name: "codex", bin: "codex", versionArgs: []string{"--version"},
			probe: []string{"exec", "--help"},
			want:  []string{"resume"},
		},
	} {
		path, err := exec.LookPath(t.bin)
		if err != nil {
			out = append(out, check{t.name, statusWarn, "not installed — sessions from this tool will not appear"})
			continue
		}
		version := strings.TrimSpace(firstLine(runCapture(t.bin, t.versionArgs...)))
		help := runCapture(t.bin, t.probe...)
		var missing []string
		for _, flagName := range t.want {
			if !strings.Contains(help, flagName) {
				missing = append(missing, flagName)
			}
		}
		switch {
		case help == "":
			out = append(out, check{t.name, statusWarn, fmt.Sprintf("%s (%s) — could not read `%s %s`",
				version, path, t.bin, strings.Join(t.probe, " "))})
		case len(missing) > 0:
			out = append(out, check{t.name, statusFail, fmt.Sprintf(
				"%s — this build expects %s, which %s no longer documents; sends may fail",
				version, strings.Join(missing, " "), t.bin)})
		default:
			out = append(out, check{t.name, statusOK, fmt.Sprintf("%s (%s)", version, path)})
		}
	}
	return out
}

// helperChecks covers the OS tools the source layer shells out to. These are
// not obvious from the README, which implies the agent CLI alone is enough.
func helperChecks() []check {
	var out []check
	for _, h := range []struct {
		bin, why string
		required bool
	}{
		{"sqlite3", "reads the OpenCode session database", true},
		{"lsof", "decides which sessions are live", true},
		{"tmux", "types into panes", false},
		{"zellij", "types into panes", false},
	} {
		path, err := exec.LookPath(h.bin)
		switch {
		case err == nil:
			out = append(out, check{h.bin, statusOK, path})
		case h.required:
			out = append(out, check{h.bin, statusWarn, "not installed — " + h.why})
		default:
			out = append(out, check{h.bin, statusOK, "not installed (optional; " + h.why + ")"})
		}
	}
	return out
}

func pathChecks(dataDir, cfgPath string) []check {
	var out []check

	if config.Exists(cfgPath) {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			out = append(out, check{"config", statusFail, cfgPath + ": " + err.Error()})
		} else if err := config.Validate(cfg); err != nil {
			out = append(out, check{"config", statusFail, cfgPath + ": " + err.Error()})
		} else {
			out = append(out, check{"config", statusOK, fmt.Sprintf("%s (%d project(s))", cfgPath, len(cfg.Projects))})
		}
	} else {
		out = append(out, check{"config", statusOK, "none yet — defaults apply (" + cfgPath + ")"})
	}
	if os.Getenv("AGENT_INBOX_CONFIG") == "" && cfgPath != filepath.Join(dataDir, "config.json") {
		out = append(out, check{"hook config", statusWarn,
			"a --config path is in use but AGENT_INBOX_CONFIG is unset, so the Stop hook will read the default instead"})
	}

	for _, d := range []struct{ name, path string }{
		{"data dir", dataDir},
		{"events dir", filepath.Join(dataDir, "events")},
	} {
		out = append(out, dirCheck(d.name, d.path))
	}
	return out
}

func dirCheck(name, path string) check {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return check{name, statusOK, path + " (will be created)"}
	}
	if err != nil {
		return check{name, statusFail, err.Error()}
	}
	if !info.IsDir() {
		return check{name, statusFail, path + " exists but is not a directory"}
	}
	// Writability is the property that matters, and the only honest way to
	// learn it is to try.
	probe := filepath.Join(path, ".doctor-write-probe")
	if err := fsutil.WriteFileAtomic(probe, []byte("ok"), fsutil.FileMode); err != nil {
		return check{name, statusFail, path + ": not writable: " + err.Error()}
	}
	os.Remove(probe)
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return check{name, statusWarn, fmt.Sprintf("%s (mode %04o — readable by other users; 0700 expected)", path, mode)}
	}
	return check{name, statusOK, fmt.Sprintf("%s (mode %04o, writable)", path, mode)}
}

// sourceChecks actually runs the configured sources, because "the adapter can
// no longer parse anything" and "there is nothing to show" need opposite
// responses from the user and look the same from the outside.
func sourceChecks() []check {
	cfg, err := sources.Load(sources.ConfigPath())
	if err != nil {
		return []check{{"sources", statusFail, err.Error()}}
	}
	built, problems := cfg.BuildWithDiagnostics()
	var out []check
	for _, p := range problems {
		out = append(out, check{"sources", statusFail, p.Error()})
	}
	if len(built) == 0 {
		out = append(out, check{"sources", statusWarn, "none usable — the inbox will always be empty"})
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agg := sources.Fetch(ctx, built)
	for _, r := range agg.Results {
		switch {
		case r.Err != nil:
			out = append(out, check{"source " + r.Source, statusFail, r.Err.Error()})
		case len(r.Warnings) > 0:
			out = append(out, check{"source " + r.Source, statusWarn,
				fmt.Sprintf("%d item(s), %d rejected: %v", len(r.Feed.Items), len(r.Warnings), r.Warnings[0])})
		default:
			out = append(out, check{"source " + r.Source, statusOK, fmt.Sprintf("%d item(s)", len(r.Feed.Items))})
		}
	}
	return out
}

func runCapture(bin string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, _ := cmd.CombinedOutput() // help text on a nonzero exit is still help text
	return string(out)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
