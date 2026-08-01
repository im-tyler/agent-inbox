// Package mux types into a terminal pane.
//
// It exists because Claude Code has no way to accept a message from outside.
// `claude --resume` is refused while a session is live, and the request for an
// injection API (anthropics/claude-code#27441) is open and unimplemented. Every
// working multi-agent setup therefore does the same thing: simulate the user
// typing into the pane. This package is that, behind one interface, for zellij
// and tmux.
//
// It is a workaround for a missing API, not a good mechanism. Everything here
// exists to make blind typing less dangerous — see Send.
package mux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Pane is one terminal pane. PID and Path are populated where the multiplexer
// reports them: tmux does, zellij does not, which is why matching a session to
// a zellij pane falls back to its title.
type Pane struct {
	ID    string
	Title string
	PID   int
	Path  string
}

type Multiplexer interface {
	Name() string
	Panes(ctx context.Context) ([]Pane, error)
	// Send types text into a pane and submits it.
	Send(ctx context.Context, paneID, text string) error
}

// submitDelay separates the text from the Enter that submits it.
//
// Without it the newline can arrive before the agent's prompt box has taken
// the text, submitting an empty or truncated message. Every tmux-based
// orchestration that works has this delay; it is not paranoia.
const submitDelay = 300 * time.Millisecond

// Detect returns the multiplexer this process is running under, or nil.
func Detect() Multiplexer {
	if os.Getenv("ZELLIJ") != "" {
		return Zellij{}
	}
	if os.Getenv("TMUX") != "" {
		return Tmux{}
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", fmt.Errorf("%s: %v: %s", name, err, detail)
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return stdout.String(), nil
}

// ── zellij ──────────────────────────────────────────────────────────────

type Zellij struct{}

func (Zellij) Name() string { return "zellij" }

// Panes parses `zellij action list-panes`, whose output is a header row then
// "PANE_ID TYPE TITLE". Zellij reports neither pid nor working directory, so
// Pane.Title is all a caller has to match on.
func (Zellij) Panes(ctx context.Context) ([]Pane, error) {
	out, err := run(ctx, "zellij", "action", "list-panes")
	if err != nil {
		return nil, err
	}
	var panes []Pane
	for i, line := range strings.Split(out, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // header
		}
		fields := strings.SplitN(line, "  ", 2)
		if len(fields) < 2 {
			continue
		}
		id := strings.TrimSpace(fields[0])
		rest := strings.TrimSpace(fields[1])
		// rest is "TYPE  TITLE"; only terminal panes can be typed into.
		parts := strings.SplitN(rest, "  ", 2)
		if len(parts) < 1 || strings.TrimSpace(parts[0]) != "terminal" {
			continue
		}
		title := ""
		if len(parts) > 1 {
			title = strings.TrimSpace(parts[1])
		}
		panes = append(panes, Pane{ID: id, Title: title})
	}
	return panes, nil
}

func (Zellij) Send(ctx context.Context, paneID, text string) error {
	// -p targets the pane directly, so the user's focus is never moved.
	if _, err := run(ctx, "zellij", "action", "write-chars", "-p", paneID, text); err != nil {
		return err
	}
	time.Sleep(submitDelay)
	_, err := run(ctx, "zellij", "action", "send-keys", "-p", paneID, "Enter")
	return err
}

// ── tmux ────────────────────────────────────────────────────────────────

// Tmux is implemented against tmux's documented interface but is UNTESTED —
// tmux was not installed on the machine this was written on. The zellij path
// is the verified one.
type Tmux struct{}

func (Tmux) Name() string { return "tmux" }

func (Tmux) Panes(ctx context.Context) ([]Pane, error) {
	const format = "#{pane_id}\t#{pane_pid}\t#{pane_current_path}\t#{pane_title}"
	out, err := run(ctx, "tmux", "list-panes", "-a", "-F", format)
	if err != nil {
		return nil, err
	}
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 4 {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		panes = append(panes, Pane{ID: f[0], PID: pid, Path: f[2], Title: f[3]})
	}
	return panes, nil
}

func (Tmux) Send(ctx context.Context, paneID, text string) error {
	// send-keys splits on newlines, so a multiline message has to go through
	// a buffer or it submits itself line by line.
	if strings.Contains(text, "\n") {
		load := exec.CommandContext(ctx, "tmux", "load-buffer", "-b", "agentinbox", "-")
		load.Stdin = strings.NewReader(text)
		if err := load.Run(); err != nil {
			return fmt.Errorf("tmux load-buffer: %w", err)
		}
		if _, err := run(ctx, "tmux", "paste-buffer", "-b", "agentinbox", "-t", paneID); err != nil {
			return err
		}
	} else if _, err := run(ctx, "tmux", "send-keys", "-t", paneID, "-l", text); err != nil {
		// -l is literal: the text is not interpreted as key names, so a
		// message containing "C-c" types those characters rather than
		// sending an interrupt.
		return err
	}
	time.Sleep(submitDelay)
	_, err := run(ctx, "tmux", "send-keys", "-t", paneID, "Enter")
	return err
}

// ── matching ────────────────────────────────────────────────────────────

// isStatusGlyph reports whether r is decoration a pane title carries rather
// than part of the title.
//
// The spinner is a braille character and cycles through the whole U+2800
// block, so enumerating the ones seen is not enough — an unlisted frame like
// U+2802 leaves the glyph attached and the title then matches nothing.
func isStatusGlyph(r rune) bool {
	if r >= 0x2800 && r <= 0x28FF { // braille patterns: the spinner frames
		return true
	}
	switch r {
	case '✳', '✻', '✽', '*', '·', '•', '⏺', '◐', '◓', '◑', '◒':
		return true
	}
	return false
}

// CleanTitle strips leading status decoration and any trailing ellipsis, so a
// pane title can be compared with a session's own title. Titles are truncated
// to the pane width, and the ellipsis left behind would otherwise defeat a
// prefix comparison.
func CleanTitle(title string) string {
	t := strings.TrimSpace(title)
	t = strings.TrimLeftFunc(t, func(r rune) bool {
		return isStatusGlyph(r) || r == ' ' || r == '\t'
	})
	t = strings.TrimSpace(t)
	for _, suffix := range []string{"…", "..."} {
		t = strings.TrimSuffix(t, suffix)
	}
	return strings.TrimSpace(t)
}

// FindByTitle returns the pane whose title matches sessionTitle, and reports
// whether the match was unambiguous.
//
// Ambiguity is the reason for the second return value rather than a nil pane:
// several panes are titled just "Claude Code" before their conversation is
// named, and typing into the wrong one sends your message to the wrong agent.
// A caller must not send on an ambiguous match.
func FindByTitle(panes []Pane, sessionTitle string) (Pane, bool) {
	want := CleanTitle(sessionTitle)
	if want == "" {
		return Pane{}, false
	}
	var hits []Pane
	for _, p := range panes {
		got := CleanTitle(p.Title)
		if got == "" {
			continue
		}
		// Pane titles are truncated to the pane width, so an exact compare
		// misses the long ones; prefix either way covers both directions.
		if got == want || strings.HasPrefix(want, got) || strings.HasPrefix(got, want) {
			hits = append(hits, p)
		}
	}
	if len(hits) == 1 {
		return hits[0], true
	}
	return Pane{}, false
}

// FindByPath returns the pane whose working directory is dir. Only tmux
// reports a path, so this returns false under zellij.
func FindByPath(panes []Pane, dir string) (Pane, bool) {
	var hits []Pane
	for _, p := range panes {
		if p.Path != "" && p.Path == dir {
			hits = append(hits, p)
		}
	}
	if len(hits) == 1 {
		return hits[0], true
	}
	return Pane{}, false
}
