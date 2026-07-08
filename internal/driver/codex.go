package driver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Codex drives OpenAI's Codex CLI in headless mode (`codex exec`).
//
// Verified against the codex CLI usage output (`codex exec --help`,
// `codex exec resume --help`). The JSONL event shape is parsed defensively
// — we only need to extract the session id, and we look for it on any
// event that carries one. If the exact event-types evolve upstream, this
// driver should still work as long as session ids appear somewhere.
//
// Last-mile verification (a real prompt run) is left to the user; the
// structure is correct against the documented CLI surface.
type Codex struct {
	Model         string // optional model override (e.g. "gpt-5", "o3")
	Sandbox       string // "read-only" | "workspace-write" | "danger-full-access"
	SkipApprovals bool   // maps to --dangerously-bypass-approvals-and-sandbox
}

func (Codex) Name() string { return "codex" }

// codexEvent is a permissive shape for one JSONL event. We capture the
// session id opportunistically and ignore the rest.
type codexEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	Item      json.RawMessage `json:"item"`
}

func (c Codex) Send(ctx context.Context, dir, sessionID, prompt string) Result {
	// tmpfile receives the agent's final message via --output-last-message.
	tmp, err := os.CreateTemp("", "agent-inbox-codex-*")
	if err != nil {
		return Result{SessionID: sessionID, Status: StatusError, Err: fmt.Errorf("codex: tempfile: %w", err)}
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	args := []string{"exec", "--json", "--output-last-message", tmpPath}
	if c.Model != "" {
		args = append(args, "-m", c.Model)
	}
	switch {
	case c.SkipApprovals:
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	case c.Sandbox != "":
		args = append(args, "-s", c.Sandbox)
	}
	if sessionID != "" {
		args = append(args, "resume", sessionID)
	}
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Dir = dir

	// Codex emits JSONL on stdout; capture stderr separately for diagnostics.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{SessionID: sessionID, Status: StatusError, Err: fmt.Errorf("codex: pipe: %w", err)}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return Result{SessionID: sessionID, Status: StatusError, Err: wrapExec(err)}
	}

	// Stream JSONL events looking for the session id. We deliberately
	// don't try to reconstruct the assistant message from events — the
	// --output-last-message file is the canonical source for that.
	var sawSessionID string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev codexEvent
		if json.Unmarshal(line, &ev) == nil && ev.SessionID != "" {
			sawSessionID = ev.SessionID
		}
	}

	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		return Result{
			SessionID: sessionID,
			Status:    StatusError,
			Err:       fmt.Errorf("codex: %v%s", err, diagSuffix(msg)),
		}
	}

	if sawSessionID != "" {
		sessionID = sawSessionID
	}

	finalBytes, readErr := os.ReadFile(tmpPath)
	if readErr != nil {
		return Result{
			SessionID: sessionID,
			Status:    StatusError,
			Err:       fmt.Errorf("codex: read last-message file: %w", readErr),
		}
	}
	final := strings.TrimSpace(string(finalBytes))
	if final == "" {
		return Result{
			SessionID: sessionID,
			Status:    StatusError,
			Err:       fmt.Errorf("codex: turn completed but no last message written"),
		}
	}
	return Result{SessionID: sessionID, Final: final, Status: StatusWaiting}
}

// AttachArgs returns the argv for interactive resume. Codex's interactive
// resume uses the bare `codex resume` subcommand (not `codex exec resume`).
func (Codex) AttachArgs(_, sessionID string) []string {
	return []string{"codex", "resume", sessionID}
}

// diagSuffix formats a stderr snippet for inclusion in an error message.
// Truncated to keep errors readable in the TUI toast line.
func diagSuffix(s string) string {
	if s == "" {
		return ""
	}
	const max = 240
	if len(s) > max {
		s = s[:max] + "…"
	}
	return "\n" + s
}
