package driver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// DefaultOpenCodeModel is a free, no-key model so OpenCode projects work
// without configuring a paid provider.
const DefaultOpenCodeModel = "opencode/deepseek-v4-flash-free"

// OpenCode drives `opencode run`. Verified against opencode 1.15.11/1.16.2:
//   - `opencode run --format json` is EMPTY on success, so we ignore run output
//     and read the reply back via `opencode export <id>` (clean structured JSON).
//   - `run` cannot create a session with a preset id, and `session list` is
//     recency-ordered, so a new session's id is found by set-difference of
//     session ids around the run, serialized via mu so only one new session is
//     created at a time (safe under concurrent projects; resumes are unlocked).
type OpenCode struct {
	Model           string
	SkipPermissions bool
	mu              *sync.Mutex
}

func NewOpenCode(model string, skipPermissions bool) *OpenCode {
	if model == "" {
		model = DefaultOpenCodeModel
	}
	return &OpenCode{Model: model, SkipPermissions: skipPermissions, mu: &sync.Mutex{}}
}

func (*OpenCode) Name() string { return "opencode" }

func (o *OpenCode) Send(ctx context.Context, dir, sessionID, prompt string) Result {
	args := []string{"run", "--model", o.Model}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	if o.SkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, prompt)

	newSession := sessionID == ""
	if newSession {
		o.mu.Lock()
		defer o.mu.Unlock()
	}

	var before map[string]bool
	if newSession {
		before = sessionIDs(ctx)
	}

	cmd := exec.CommandContext(ctx, "opencode", args...)
	cmd.Dir = dir
	runOut, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return Result{SessionID: sessionID, Status: StatusError, Err: fmt.Errorf("opencode run: %v: %s", runErr, strings.TrimSpace(string(runOut)))}
	}

	if newSession {
		id, err := newSessionID(ctx, before)
		if err != nil {
			return Result{Status: StatusError, Err: err}
		}
		sessionID = id
	}

	text, errMsg, err := exportLastAssistant(ctx, sessionID)
	if err != nil {
		// Export failed — session may be stale or format changed.
		// Fall back to whatever the run produced so the user sees
		// SOMETHING instead of a cryptic JSON parse error.
		runText := strings.TrimSpace(string(runOut))
		if runText != "" {
			return Result{SessionID: sessionID, Final: runText, Status: StatusWaiting}
		}
		return Result{SessionID: sessionID, Status: StatusError, Err: err}
	}
	if errMsg != "" && text == "" {
		return Result{SessionID: sessionID, Status: StatusError, Err: errors.New(errMsg)}
	}
	return Result{SessionID: sessionID, Final: text, Status: StatusWaiting}
}

func (*OpenCode) AttachArgs(dir, sessionID string) []string {
	return []string{"opencode", "run", "-i", "--session", sessionID}
}

func sessionIDs(ctx context.Context) map[string]bool {
	ids := map[string]bool{}
	out, err := exec.CommandContext(ctx, "opencode", "session", "list").Output()
	if err != nil {
		return ids
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) > 0 && strings.HasPrefix(f[0], "ses_") {
			ids[f[0]] = true
		}
	}
	return ids
}

func newSessionID(ctx context.Context, before map[string]bool) (string, error) {
	for id := range sessionIDs(ctx) {
		if !before[id] {
			return id, nil
		}
	}
	return "", errors.New("opencode: could not determine new session id")
}

type ocExport struct {
	Messages []struct {
		Info struct {
			Role  string          `json:"role"`
			Error json.RawMessage `json:"error"`
		} `json:"info"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"messages"`
}

func exportLastAssistant(ctx context.Context, sessionID string) (text, errMsg string, err error) {
	out, e := exec.CommandContext(ctx, "opencode", "export", sessionID).Output()
	if e != nil {
		return "", "", wrapExec(e)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return "", "", fmt.Errorf("opencode export returned no data (session %q may not exist)", sessionID)
	}
	if i := bytes.IndexByte(out, '{'); i > 0 { // strip "Exporting session: ..." prefix
		out = out[i:]
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return "", "", fmt.Errorf("opencode export returned no JSON for session %q", sessionID)
	}
	var ex ocExport
	if e := json.Unmarshal(out, &ex); e != nil {
		return "", "", fmt.Errorf("parse opencode export: %w", e)
	}
	for i := len(ex.Messages) - 1; i >= 0; i-- {
		m := ex.Messages[i]
		if m.Info.Role != "assistant" {
			continue
		}
		var sb strings.Builder
		for _, p := range m.Parts {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		if len(m.Info.Error) > 0 && string(m.Info.Error) != "null" {
			var oe struct {
				Data struct {
					Message string `json:"message"`
				} `json:"data"`
			}
			_ = json.Unmarshal(m.Info.Error, &oe)
			errMsg = oe.Data.Message
		}
		return strings.TrimSpace(sb.String()), errMsg, nil
	}
	return "", "", fmt.Errorf("opencode: no assistant message found in session %q", sessionID)
}
