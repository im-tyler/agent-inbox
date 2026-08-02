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

	in.mu.Lock()
	king, err := in.project(kingIdx)
	if err != nil {
		in.mu.Unlock()
		return err
	}
	kingName := king.Name
	in.mu.Unlock()

	if err := in.sendRaw(kingIdx, prompt, driverPrompt); err != nil {
		return err
	}

	// From here on the king is identified by name. A watcher lives for
	// minutes, and RemoveProject shifts every index after the one it drops —
	// an index held that long eventually names a different project.
	in.track(func() { in.kingDispatchWatcher(kingName) })
	return nil
}

// awaitKing polls until the king's turn ends, returning its response. A turn
// that was cancelled (Idle) or errored is not a response: acting on one would
// mean dispatching work off a failure.
func (in *Inbox) awaitKing(kingName string, deadline time.Time) (string, bool) {
	for time.Now().Before(deadline) {
		if !in.pause() {
			return "", false
		}
		in.mu.Lock()
		p, err := in.projectByName(kingName)
		if err != nil {
			in.mu.Unlock()
			return "", false
		}
		if p.Status == driver.StatusWorking {
			in.mu.Unlock()
			continue
		}
		response := p.LastMessage
		ok := p.Status == driver.StatusWaiting
		in.mu.Unlock()
		return response, ok
	}
	return "", false
}

// kingDispatchWatcher waits for the king's turn, records any notes it took,
// then parses the response for [send to X: Y] directives and dispatches them.
func (in *Inbox) kingDispatchWatcher(kingName string) {
	response, ok := in.awaitKing(kingName, time.Now().Add(kingRoundTimeout))
	if !ok {
		return
	}
	in.applyNoteDirectives(response)

	var sent []string
	for _, d := range ParseKingDirectives(response) {
		// A failed dispatch must not get a watcher. Otherwise the watcher
		// waits out whatever that project was already doing and files its
		// unrelated answer as the reply to this question — confident,
		// stale, and wrong. An unknown name and a busy project both land
		// here, and both mean the same thing to the user: nothing was sent.
		if err := in.sendNamed(d.Target, d.Message, d.Message, true); err != nil {
			in.noteToKing(kingName, fmt.Sprintf("%s: %v — nothing sent", d.Target, err))
			continue
		}
		sent = append(sent, d.Target)
	}
	if len(sent) > 0 {
		in.track(func() { in.kingRoundWatcher(kingName, sent) })
	}
}

// applyNoteDirectives records what the king chose to remember and retracts
// what it chose to forget. Drops run first: retracting and restating a fact in
// one turn is how a correction is phrased.
func (in *Inbox) applyNoteDirectives(response string) {
	in.DropNotes(ParseKingNoteDrops(response))
	in.AddNotes(ParseKingNotes(response))
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

	// Notes lead. They are what you knew before this turn, and a fact you
	// already established should not be re-derived from a status line. Only
	// the ones about this fleet: a note naming a project you are not talking
	// to is context spent on nothing.
	lowerNames := make(map[string]bool, len(nameSet))
	for n := range nameSet {
		lowerNames[strings.ToLower(n)] = true
	}
	if notes := in.NotesFor(lowerNames); len(notes) > 0 {
		b.WriteString("What you have noted about this fleet:\n")
		for _, n := range notes {
			b.WriteString("- " + n.Text + "\n")
		}
		b.WriteString("\n")
	}

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
		b.WriteString("\nTo remember something across turns, output a line of its own:\n")
		b.WriteString("[note: teploy depends on Neutron's DB layer]\n\n")
		b.WriteString("Note only durable facts that span projects or would cost a round-trip to rediscover.")
		b.WriteString(" Not status — you are given that fresh every turn.\n")
		b.WriteString("When a note above turns out to be wrong or out of date, retract it:\n")
		b.WriteString("[note drop: teploy depends on Neutron]\n")
		b.WriteString("The text just has to match part of the note. Retract and restate to correct one.\n")
		b.WriteString("Everything else in your response is shown to the user.\n")
	}
	return b.String()
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

// defaultPollEvery is how often a round checks its targets. It lives on the
// Inbox rather than in a package var: watchers outlive the turn that started
// them, so a test that reset a global would be writing it while a live
// goroutine still read it.
const defaultPollEvery = 500 * time.Millisecond

func (in *Inbox) pollInterval() time.Duration {
	if in.pollEvery > 0 {
		return in.pollEvery
	}
	return defaultPollEvery
}

// kingRoundWatcher waits for every dispatched project, files a one-line
// receipt for each in the king's thread, then hands the full replies back to
// the king so it can report to the user.
//
// The summary turn is sent directly rather than through KingSend, so any
// directives in it are not dispatched. That is deliberate: it is what stops a
// king from answering its own summary with more work, forever.
func (in *Inbox) kingRoundWatcher(kingName string, targets []string) {
	deadline := time.Now().Add(kingRoundTimeout)

	replies := make([]fleetReply, 0, len(targets))
	for _, name := range targets {
		if r, ok := in.awaitReply(name, deadline); ok {
			replies = append(replies, r)
		} else {
			replies = append(replies, fleetReply{
				name:    name,
				content: "(no reply within the round timeout)",
				failed:  true,
			})
		}
	}

	in.mu.Lock()
	king, err := in.projectByName(kingName)
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

	if err := in.sendNamed(kingName, "", summaryPrompt(replies), false); err != nil {
		in.noteToKing(kingName, fmt.Sprintf("fleet replies arrived but the summary could not start: %v", err))
		return
	}
	// The summary is where the cross-project facts actually surface, so it is
	// the turn most worth harvesting notes from.
	if response, ok := in.awaitKing(kingName, time.Now().Add(kingRoundTimeout)); ok {
		in.applyNoteDirectives(response)
	}
}

// awaitReply polls one target until its turn ends or the round runs out.
func (in *Inbox) awaitReply(name string, deadline time.Time) (fleetReply, bool) {
	for time.Now().Before(deadline) {
		if !in.pause() {
			return fleetReply{}, false
		}
		in.mu.Lock()
		target, err := in.projectByName(name)
		if err != nil {
			in.mu.Unlock()
			return fleetReply{}, false
		}
		if target.Status == driver.StatusWorking {
			in.mu.Unlock()
			continue
		}
		reply := fleetReply{name: name, content: target.LastMessage}
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
func (in *Inbox) noteToKing(kingName, text string) {
	in.mu.Lock()
	king, err := in.projectByName(kingName)
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
