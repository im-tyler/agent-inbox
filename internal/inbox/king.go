package inbox

import (
	"bufio"
	"fmt"
	"strings"
	"time"

	"agentinbox/internal/driver"
)

// KingDirective is a parsed [send to X: Y] line from the king's response.
type KingDirective struct {
	Target  string
	Message string
}

// KingSend dispatches a prompt to the king project with the current state
// of connected projects injected into the prompt. After the king responds,
// any [send to X: Y] directives in the response are parsed and dispatched
// to the target projects via normal Send.
//
// The king itself is just a regular project (any tool). The "king" behavior
// is purely in how the prompt is constructed and how the response is parsed.
//
// This is the Layer 1 king: state-injected prompts, directive-based dispatch,
// no persistent event loop. The king sees fresh state on every turn.
func (in *Inbox) KingSend(kingIdx int, prompt string, connectedNames []string) error {
	// If there are no connected projects, this is just a normal chat —
	// no state injection, no directive parsing. The king is a regular
	// project session.
	if len(connectedNames) == 0 {
		return in.sendRaw(kingIdx, prompt, prompt)
	}

	// Build compact fleet state (one line per project, no verbose instructions).
	stateCtx := in.formatKingState(connectedNames)
	driverPrompt := prompt + "\n\n---\n\n" + stateCtx

	if err := in.sendRaw(kingIdx, prompt, driverPrompt); err != nil {
		return err
	}

	go in.kingDispatchWatcher(kingIdx)
	return nil
}

// kingDispatchWatcher polls the king's status until it's no longer Working,
// then parses the response for [send to X: Y] directives and dispatches them.
func (in *Inbox) kingDispatchWatcher(kingIdx int) {
	// Wait for the king to finish.
	for {
		time.Sleep(500 * time.Millisecond)
		in.mu.Lock()
		p, err := in.project(kingIdx)
		if err != nil {
			in.mu.Unlock()
			return
		}
		if p.Status != driver.StatusWorking {
			// Only dispatch directives when the king completed successfully
			// (StatusWaiting). If cancelled (Idle) or errored (Error), skip —
			// we don't want to act on stale or failed responses.
			if p.Status != driver.StatusWaiting {
				in.mu.Unlock()
				return
			}
			response := p.LastMessage
			in.mu.Unlock()

			// Parse and dispatch directives.
			directives := ParseKingDirectives(response)
			var sent []dispatched
			for _, d := range directives {
				idx := in.findProjectByName(d.Target)
				if idx == 0 {
					in.noteToKing(kingIdx, fmt.Sprintf("no project named %q — nothing sent", d.Target))
					continue
				}
				// A failed dispatch must not get a watcher. Otherwise the
				// watcher waits out whatever that project was already doing
				// and files its unrelated answer as the reply to this
				// question — confident, stale, and wrong.
				if err := in.Send(idx, d.Message); err != nil {
					in.noteToKing(kingIdx, fmt.Sprintf("%s: %v — nothing sent", d.Target, err))
					continue
				}
				sent = append(sent, dispatched{name: d.Target, idx: idx})
			}
			if len(sent) > 0 {
				go in.kingRoundWatcher(kingIdx, sent)
			}
			return
		}
		in.mu.Unlock()
	}
}

// formatKingState builds compact fleet context for the king's prompt.
// Includes a concrete directive example using a real project name so
// weaker models can copy the format instead of guessing.
func (in *Inbox) formatKingState(connectedNames []string) string {
	snap := in.Snapshot()

	nameSet := make(map[string]bool, len(connectedNames))
	for _, n := range connectedNames {
		nameSet[n] = true
	}

	var b strings.Builder
	var firstProject string
	b.WriteString("Your fleet:\n")
	found := false
	for _, p := range snap {
		if !nameSet[p.Name] {
			continue
		}
		found = true
		if firstProject == "" {
			firstProject = p.Name
		}
		status := string(p.Status)
		if p.Activity != "" {
			status += ":" + p.Activity
		}
		lastMsg := truncateForKing(p.LastMessage, 80)
		if lastMsg == "" {
			if p.LastErr != "" {
				lastMsg = "error: " + truncateForKing(p.LastErr, 60)
			} else {
				lastMsg = "no recent activity"
			}
		}
		b.WriteString(fmt.Sprintf("- %s (%s) [%s]: %s\n", p.Name, p.Tool, status, lastMsg))
	}
	if found && firstProject != "" {
		// Stating the format was not enough. Asked about another project, a
		// model reaches for the filesystem first — and every one of those
		// calls is rejected, because a fleet project's folder is outside the
		// king's working directory. So say what is impossible, not just what
		// is available.
		b.WriteString("\nYou cannot read these projects' files or run commands in their directories.")
		b.WriteString(" They are outside your working directory and every such attempt is rejected.")
		b.WriteString(" Each project is a live agent session in its own folder, and asking it is the only way to learn anything about it.\n")
		b.WriteString("\nTo ask a project something, or give it a task, output this exact format on its own line:\n")
		b.WriteString(fmt.Sprintf("[send to %s: describe the task here]\n\n", firstProject))
		b.WriteString(fmt.Sprintf("Example: [send to %s: what are you working on?]\n", firstProject))
		b.WriteString("You can include multiple [send to ...] lines — they run in parallel, and you get every reply back before you answer the user.\n")
		b.WriteString("Everything else in your response is shown to the user.\n")
	}
	return b.String()
}

// findProjectByName returns the 1-based index of the project with the given
// name, or 0 if not found.
func (in *Inbox) findProjectByName(name string) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	for i, p := range in.projects {
		if strings.EqualFold(p.Name, name) {
			return i + 1
		}
	}
	return 0
}

// ParseKingDirectives extracts [send to X: Y] directives from a response.
// Exported so it can be tested independently.
func ParseKingDirectives(response string) []KingDirective {
	var dirs []KingDirective
	sc := bufio.NewScanner(strings.NewReader(response))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		lower := strings.ToLower(line)
		if !strings.HasPrefix(lower, "[send to ") {
			continue
		}
		rest := line[9:] // skip "[send to "
		if !strings.HasSuffix(rest, "]") {
			continue
		}
		rest = strings.TrimSuffix(rest, "]")
		colonIdx := strings.Index(rest, ":")
		if colonIdx < 0 {
			continue
		}
		target := strings.TrimSpace(rest[:colonIdx])
		message := strings.TrimSpace(rest[colonIdx+1:])
		if target != "" && message != "" {
			dirs = append(dirs, KingDirective{Target: target, Message: message})
		}
	}
	return dirs
}

// dispatched is one project the king successfully sent a task to.
type dispatched struct {
	name string
	idx  int
}

// fleetReply is what a dispatched project came back with.
type fleetReply struct {
	name    string
	content string
	failed  bool
}

const (
	// kingRoundTimeout bounds a round. A project whose driver never returns
	// would otherwise hold the summary — and this goroutine — forever.
	kingRoundTimeout = 15 * time.Minute
	// receiptWidth is how much of a reply the king's thread shows. The full
	// text is in that project's own thread; repeating it here would make the
	// supervisor's conversation the transcript of every other one.
	receiptWidth = 100
)

// kingPollEvery is how often a round checks its targets. A var so tests can
// run a full round without waiting on real polling intervals.
var kingPollEvery = 500 * time.Millisecond

// kingRoundWatcher waits for every dispatched project, files a one-line
// receipt for each in the king's thread, then hands the full replies back to
// the king so it can report to the user.
//
// The summary turn is sent directly rather than through KingSend, so any
// directives in it are not dispatched. That is deliberate: it is what stops a
// king from answering its own summary with more work, forever.
func (in *Inbox) kingRoundWatcher(kingIdx int, targets []dispatched) {
	deadline := time.Now().Add(kingRoundTimeout)

	replies := make([]fleetReply, 0, len(targets))
	for _, t := range targets {
		if r, ok := in.awaitReply(t, deadline); ok {
			replies = append(replies, r)
		} else {
			replies = append(replies, fleetReply{
				name:    t.name,
				content: "(no reply within the round timeout)",
				failed:  true,
			})
		}
	}

	in.mu.Lock()
	king, err := in.project(kingIdx)
	if err != nil {
		in.mu.Unlock()
		return
	}
	for _, r := range replies {
		king.appendHistory(Message{
			Role:      r.name,
			Content:   truncateForKing(r.content, receiptWidth),
			Timestamp: time.Now(),
		})
	}
	in.mu.Unlock()
	in.save()

	if err := in.sendInternal(kingIdx, "", summaryPrompt(replies), false); err != nil {
		in.noteToKing(kingIdx, fmt.Sprintf("fleet replies arrived but the summary could not start: %v", err))
	}
}

// awaitReply polls one target until its turn ends or the round runs out.
func (in *Inbox) awaitReply(t dispatched, deadline time.Time) (fleetReply, bool) {
	for time.Now().Before(deadline) {
		time.Sleep(kingPollEvery)
		in.mu.Lock()
		target, err := in.project(t.idx)
		if err != nil {
			in.mu.Unlock()
			return fleetReply{}, false
		}
		if target.Status == driver.StatusWorking {
			in.mu.Unlock()
			continue
		}
		reply := fleetReply{name: t.name, content: target.LastMessage}
		if target.LastErr != "" {
			reply.content = "(error: " + target.LastErr + ")"
			reply.failed = true
		}
		if reply.content == "" {
			reply.content = "(no output)"
			reply.failed = true
		}
		in.mu.Unlock()
		return reply, true
	}
	return fleetReply{}, false
}

// summaryPrompt hands the king the full replies it dispatched for.
func summaryPrompt(replies []fleetReply) string {
	var b strings.Builder
	b.WriteString("The projects you dispatched have replied. Their full responses:\n\n")
	for _, r := range replies {
		b.WriteString("--- " + r.name + " ---\n")
		b.WriteString(r.content)
		b.WriteString("\n\n")
	}
	b.WriteString("Report back to the user: what each project found, and what it means taken together. ")
	b.WriteString("Do not emit [send to ...] directives in this reply — they will not be dispatched.\n")
	return b.String()
}

// noteToKing records a system line in the king's thread. Used for the things
// the user has to know but no agent said: a dispatch that never happened, a
// round that timed out.
func (in *Inbox) noteToKing(kingIdx int, text string) {
	in.mu.Lock()
	king, err := in.project(kingIdx)
	if err != nil {
		in.mu.Unlock()
		return
	}
	king.appendHistory(Message{Role: "system", Content: text, Timestamp: time.Now()})
	in.mu.Unlock()
	in.save()
}

func truncateForKing(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
