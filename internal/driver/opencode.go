package driver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultOpenCodeModel is a free, no-key model so OpenCode projects work
// without configuring a paid provider.
const DefaultOpenCodeModel = "opencode/deepseek-v4-flash-free"

// OpenCode drives `opencode run`.
//
// There are two paths, and the streaming one in opencode_stream.go is the one
// that normally runs — the inbox prefers StreamSend wherever a driver offers
// it. What follows is the blocking path, which is still reached when a turn
// cannot stream.
//
// Verified against opencode 1.15.11/1.16.2 and 1.18.11:
//   - `opencode run --format json` was EMPTY on success, so this path ignores
//     run output and reads the reply back via `opencode export <id>`. As of
//     1.18.18 that is no longer true — it emits NDJSON events — which is what
//     the streaming path reads, and why it needs none of the recovery below.
//   - `run` cannot create a session with a preset id, and `session list` is
//     recency-ordered, so a new session's id is found by set-difference of
//     session ids around the run, serialized via mu so only one new session is
//     created at a time (safe under concurrent projects; resumes are unlocked).
//     The streaming path does not need this either: every event it reads
//     carries the session id.
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
		// --auto is the documented spelling. --dangerously-skip-permissions
		// still works — 1.18.15 keeps it as a hidden option wired to the same
		// value — but a hidden alias is a removal candidate and the documented
		// name is the one that will survive.
		args = append(args, "--auto")
	}
	args = append(args, prompt)

	newSession := sessionID == ""
	if newSession {
		o.mu.Lock()
		defer o.mu.Unlock()
	}

	// On a resume, remember which assistant message was last before the run.
	// Export is what supplies the reply, and it can return a *successful* view
	// of state that has not caught up yet — so "export worked" is not the same
	// as "export shows this turn". Without a marker the previous turn's answer
	// is accepted as the answer to this prompt.
	var priorMsgID string
	if !newSession {
		if _, id, _, err := exportLastAssistant(ctx, sessionID); err == nil {
			priorMsgID = id
		}
	}

	var before map[string]bool
	if newSession {
		var err error
		before, err = sessionIDs(ctx, dir)
		if err != nil {
			// Without a reliable before-set, every historical session looks
			// new and the set difference would pick one of them at random.
			// Refusing to start beats attaching this project to somebody
			// else's conversation.
			return Result{Status: StatusError,
				Err: fmt.Errorf("opencode: cannot list sessions, so a new one could not be identified: %w", err)}
		}
	}

	// stdout and stderr are kept apart. They were merged, so opencode's
	// diagnostics ended up inside the reply on the recovery path below —
	// stderr belongs in an error message, not in what the agent said.
	cmd := startProcess(ctx, "opencode", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if runErr := cmd.Run(); runErr != nil {
		return Result{SessionID: sessionID, Status: StatusError,
			Err: fmt.Errorf("opencode run: %v%s", runErr, diagSuffix(strings.TrimSpace(stderr.String())))}
	}
	runOut := stdout.Bytes()

	if newSession {
		id, err := newSessionID(ctx, dir, before, prompt)
		if err != nil {
			// Session state is written asynchronously after run returns, so
			// the list may not have caught up. One more try.
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return Result{Status: StatusError, Err: ctx.Err()}
			}
			id, err = newSessionID(ctx, dir, before, prompt)
			if err != nil {
				return Result{Status: StatusError, Err: fmt.Errorf("opencode: run succeeded but can't determine session id: %w", err)}
			}
		}
		sessionID = id
	}

	// Try export with retry — the session may not be immediately exportable,
	// and on a resume it may not yet show this turn.
	text, errMsg, err := exportWithRetry(ctx, sessionID, exportAttempts, priorMsgID)
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

// sessionIDs lists the session ids visible from dir.
//
// The error is returned rather than swallowed into an empty map: an empty map
// and a failed listing mean opposite things to the set-difference that
// identifies a new session.
func sessionIDs(ctx context.Context, dir string) (map[string]bool, error) {
	ids := map[string]bool{}
	cmd := startProcess(ctx, "opencode", "session", "list")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, wrapExec(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) > 0 && strings.HasPrefix(f[0], "ses_") {
			ids[f[0]] = true
		}
	}
	return ids, nil
}

// newSessionID identifies the session the run just created.
//
// Set difference alone is not enough. The mutex serialises this process, but
// another opencode running in the same project — which this product exists to
// coexist with — can create a session in the same window. Two new ids then
// appear and map iteration picked one arbitrarily, so the project could be
// bound to a stranger's session.
//
// With more than one candidate, ask each what its first user message was and
// take the one that matches the prompt we sent. No match, or several, is an
// identity error rather than a guess.
func newSessionID(ctx context.Context, dir string, before map[string]bool, prompt string) (string, error) {
	now, err := sessionIDs(ctx, dir)
	if err != nil {
		return "", err
	}
	var candidates []string
	for id := range now {
		if !before[id] {
			candidates = append(candidates, id)
		}
	}
	switch len(candidates) {
	case 0:
		return "", errors.New("opencode: no new session appeared")
	case 1:
		return candidates[0], nil
	}
	// Deterministic order so the error message is stable across runs.
	sort.Strings(candidates)
	var matched []string
	for _, id := range candidates {
		if firstUserMessage(ctx, id) == strings.TrimSpace(prompt) {
			matched = append(matched, id)
		}
	}
	if len(matched) == 1 {
		return matched[0], nil
	}
	return "", fmt.Errorf("opencode: %d sessions appeared during this run (%s) and %d match the prompt — refusing to guess",
		len(candidates), strings.Join(candidates, ", "), len(matched))
}

// firstUserMessage is the text of a session's opening user turn, used to
// correlate a session with the prompt that created it.
func firstUserMessage(ctx context.Context, sessionID string) string {
	ex, err := exportSession(ctx, sessionID)
	if err != nil {
		return ""
	}
	for _, m := range ex.Messages {
		if m.Info.Role != "user" {
			continue
		}
		var sb strings.Builder
		for _, p := range m.Parts {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

type ocExport struct {
	Messages []struct {
		Info struct {
			ID    string          `json:"id"`
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
//
// priorMsgID, when set, is the assistant message that was last *before* this
// turn ran. Retrying only on error was not enough: opencode persists
// asynchronously, so export can succeed and return the previous turn's answer,
// which was then filed as the reply to the new prompt. Text comparison cannot
// substitute for this — two turns can legitimately produce identical text —
// so the check is on the message's identity.
func exportWithRetry(ctx context.Context, sessionID string, attempts int, priorMsgID string) (text, errMsg string, err error) {
	wait := exportBackoff
	var id string
	for i := 0; i < attempts; i++ {
		text, id, errMsg, err = exportLastAssistant(ctx, sessionID)
		if err == nil && (priorMsgID == "" || id != priorMsgID) {
			return text, errMsg, nil
		}
		if err == nil {
			err = fmt.Errorf("opencode export still shows the previous turn (message %s)", id)
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

// exportSession runs `opencode export` and parses it.
func exportSession(ctx context.Context, sessionID string) (ocExport, error) {
	var ex ocExport
	out, e := startProcess(ctx, "opencode", "export", sessionID).Output()
	if e != nil {
		return ex, wrapExec(e)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return ex, fmt.Errorf("opencode export returned no data (session %q may not exist)", sessionID)
	}
	if i := bytes.IndexByte(out, '{'); i > 0 { // strip "Exporting session: ..." prefix
		out = out[i:]
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return ex, fmt.Errorf("opencode export returned no JSON for session %q", sessionID)
	}
	if e := json.Unmarshal(out, &ex); e != nil {
		return ex, fmt.Errorf("parse opencode export: %w", e)
	}
	return ex, nil
}

// exportLastAssistant returns the last assistant turn's text and its message
// id. The id is what lets a caller tell this turn from the one before it.
func exportLastAssistant(ctx context.Context, sessionID string) (text, msgID, errMsg string, err error) {
	ex, err := exportSession(ctx, sessionID)
	if err != nil {
		return "", "", "", err
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
		return strings.TrimSpace(sb.String()), m.Info.ID, errMsg, nil
	}
	return "", "", "", fmt.Errorf("opencode: no assistant message found in session %q", sessionID)
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
	if len(lines) > 0 && isOpenCodeBanner(lines[0]) {
		lines = lines[1:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// isOpenCodeBanner recognises opencode's "> build · model" status line.
//
// It matches the banner's actual grammar rather than "starts with '> '". The
// looser test removed any first line that began a Markdown blockquote, so an
// answer opening with a quotation — a perfectly ordinary way for an agent to
// begin — silently lost its first line.
func isOpenCodeBanner(line string) bool {
	t := strings.TrimSpace(line)
	rest, ok := strings.CutPrefix(t, "> ")
	if !ok {
		return false
	}
	// The banner is fields joined by "·": the build, then the model, and
	// nothing that reads like prose. A quotation has no middle dot in it.
	if !strings.Contains(rest, "·") {
		return false
	}
	for _, part := range strings.Split(rest, "·") {
		part = strings.TrimSpace(part)
		if part == "" {
			return false
		}
		// Banner fields are single tokens (a version, a provider/model path).
		// Anything with a space in it is a sentence, not a field.
		if strings.ContainsAny(part, " \t") {
			return false
		}
	}
	return true
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
