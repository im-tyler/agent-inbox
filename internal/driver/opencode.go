package driver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultOpenCodeModel is a free, no-key model so OpenCode projects work
// without configuring a paid provider.
const DefaultOpenCodeModel = "opencode/deepseek-v4-flash-free"

// OpenCode drives `opencode run`. Verified against opencode 1.15.11/1.16.2
// and 1.18.11:
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
		before = sessionIDs(ctx, dir)
	}

	// stdout and stderr are kept apart. They were merged, so opencode's
	// diagnostics ended up inside the reply on the recovery path below —
	// stderr belongs in an error message, not in what the agent said.
	cmd := exec.CommandContext(ctx, "opencode", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if runErr := cmd.Run(); runErr != nil {
		return Result{SessionID: sessionID, Status: StatusError,
			Err: fmt.Errorf("opencode run: %v%s", runErr, diagSuffix(strings.TrimSpace(stderr.String())))}
	}
	runOut := stdout.Bytes()

	if newSession {
		id, err := newSessionID(ctx, dir, before)
		if err != nil {
			// Session was created upstream but we can't determine the ID.
			// Try once more after a brief delay — the session list might
			// not have updated yet.
			time.Sleep(time.Second)
			id, err = newSessionID(ctx, dir, before)
			if err != nil {
				return Result{Status: StatusError, Err: fmt.Errorf("opencode: run succeeded but can't determine session id: %w", err)}
			}
		}
		sessionID = id
	}

	// Try export with retry — the session may not be immediately exportable.
	text, errMsg, err := exportWithRetry(ctx, sessionID, exportAttempts)
	if err != nil {
		if runText := recoveredReply(string(runOut)); runText != "" {
			return Result{SessionID: sessionID, Final: runText, Status: StatusWaiting}
		}
		return Result{SessionID: sessionID, Status: StatusError, Err: err}
	}
	if errMsg != "" && text == "" {
		return Result{SessionID: sessionID, Status: StatusError, Err: errors.New(errMsg)}
	}
	return Result{SessionID: sessionID, Final: cleanReply(text), Status: StatusWaiting}
}

func (*OpenCode) AttachArgs(dir, sessionID string) []string {
	return []string{"opencode", "run", "-i", "--session", sessionID}
}

func sessionIDs(ctx context.Context, dir string) map[string]bool {
	ids := map[string]bool{}
	cmd := exec.CommandContext(ctx, "opencode", "session", "list")
	cmd.Dir = dir
	out, err := cmd.Output()
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

func newSessionID(ctx context.Context, dir string, before map[string]bool) (string, error) {
	for id := range sessionIDs(ctx, dir) {
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

const (
	// exportAttempts and exportBackoff bound how long we wait for opencode to
	// make a session exportable.
	//
	// This was 3 attempts 500ms apart — 1.5s — and that is why the recovery
	// path below existed in practice rather than in theory. opencode writes
	// session state asynchronously after `run` returns, so a freshly created
	// session routinely was not exportable inside that window, every early
	// turn fell through to scraping the terminal, and the transcript got
	// filed as the agent's reply. Doubling from 400ms gives ~6s across six
	// tries, which costs nothing on the normal path: the first attempt
	// succeeds and none of the sleeps happen.
	exportAttempts = 6
	exportBackoff  = 400 * time.Millisecond
)

// exportWithRetry calls exportLastAssistant, backing off between attempts.
func exportWithRetry(ctx context.Context, sessionID string, attempts int) (text, errMsg string, err error) {
	wait := exportBackoff
	for i := 0; i < attempts; i++ {
		text, errMsg, err = exportLastAssistant(ctx, sessionID)
		if err == nil {
			return text, errMsg, nil
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			// A cancelled turn should not go on sleeping through its budget.
			return "", "", ctx.Err()
		}
		wait *= 2
	}
	return "", "", err
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

// cleanReply drops opencode's "> build · model" banner, which is meant for
// someone watching a terminal and in a chat transcript reads as the agent's
// first words. It runs on both paths: the banner turns up in exported
// assistant text too, not only in the raw stdout the fallback uses.
//
// Only the banner goes. The "!" and "✗" lines stay: when the fallback is what
// you are reading, a rejected tool call is usually the reason the turn went
// the way it did, and dropping it would leave an inexplicable answer.
func cleanReply(s string) string {
	lines := strings.Split(stripANSI(s), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "> ") {
		lines = lines[1:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ansiPattern matches CSI escape sequences — the colour and cursor codes
// opencode writes for a human watching a terminal.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// stripANSI removes terminal control codes.
//
// It runs before every other check, because those checks are about the text
// and an escape sequence is not text. The banner test in particular is a
// prefix test, and the banner arrives as "\x1b[0m\n> build · model" — escape
// characters are not whitespace, so TrimSpace left them in place, the prefix
// never matched, and both the banner and the raw codes reached the UI.
func stripANSI(s string) string { return ansiPattern.ReplaceAllString(s, "") }

// maxRecovered caps a recovered transcript. It is injected into the king's
// context and rendered as a sidebar preview, and the whole terminal output of
// a tool-using turn ran to twelve thousand characters in practice.
const maxRecovered = 1500

// recoveredReply salvages something readable from raw terminal output when
// `opencode export` could not be read.
//
// This is not the reply and cannot be made into one: what opencode prints is a
// transcript, with the assistant's prose interleaved with shell commands it
// echoed and their output, and no marker separating them. Guessing which lines
// were speech would sometimes be wrong silently.
//
// So it says what it is. The command echoes and their output go, because those
// are the bulk and they are certainly not speech; what remains is capped and
// labelled, so a reader knows they are looking at scrapings rather than an
// answer, and knows to open the session for the real one.
func recoveredReply(raw string) string {
	cleaned := cleanReply(raw)
	if cleaned == "" {
		return ""
	}

	var kept []string
	inCommand := false
	for _, line := range strings.Split(cleaned, "\n") {
		trimmed := strings.TrimSpace(line)
		// "$ " starts an echoed shell command; everything up to the next
		// blank line is that command and its output.
		if strings.HasPrefix(trimmed, "$ ") {
			inCommand = true
			continue
		}
		if inCommand {
			if trimmed == "" {
				inCommand = false
			}
			continue
		}
		kept = append(kept, line)
	}

	body := strings.TrimSpace(strings.Join(kept, "\n"))
	if body == "" {
		return ""
	}
	if r := []rune(body); len(r) > maxRecovered {
		body = strings.TrimSpace(string(r[:maxRecovered])) + "\n[…truncated]"
	}
	return body + "\n\n(recovered from terminal output — opencode export was not ready; open the session for the full reply)"
}
