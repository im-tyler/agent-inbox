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
			for _, d := range directives {
				if idx := in.findProjectByName(d.Target); idx > 0 {
					_ = in.Send(idx, d.Message)
				}
			}
			return
		}
		in.mu.Unlock()
	}
}

// formatKingState builds compact fleet context for the king's prompt.
// One line per connected project. No verbose instructions — modern LLMs
// can infer the [send to NAME: message] directive format from context.
func (in *Inbox) formatKingState(connectedNames []string) string {
	snap := in.Snapshot()

	nameSet := make(map[string]bool, len(connectedNames))
	for _, n := range connectedNames {
		nameSet[n] = true
	}

	var b strings.Builder
	b.WriteString("Other projects in your fleet:\n")
	found := false
	for _, p := range snap {
		if !nameSet[p.Name] {
			continue
		}
		found = true
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
	if found {
		b.WriteString("\nTo delegate work to a project, write: [send to NAME: your message]\n")
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

func truncateForKing(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
