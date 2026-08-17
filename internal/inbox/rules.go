package inbox

import (
	"fmt"
	"strings"

	"github.com/im-tyler/agent-inbox/internal/config"
)

// Standing rules, and who is allowed to write one.
//
// The supervisor's replies are shaped by what its projects say, and what its
// projects say is shaped by the repositories, issues and web pages those agents
// read. Everything the supervisor writes is therefore untrusted-derived, and
// per-turn provenance does not help: its session is continuous, so once the
// fleet is non-empty every later turn has untrusted text somewhere behind it.
//
// What does separate cleanly is the principal. The user is the one; the
// supervisor is an agent acting for them over data it cannot vouch for. So the
// supervisor proposes and the user ratifies, and a ratified rule lives in
// config.json — the file the user owns, where every other policy in this
// program already lives.
//
// This is the stance the dispatcher has always taken on actions, applied to
// beliefs. dispatchDirectives enforces an allowlist rather than trusting the
// response, because a target named in model output is a name and not
// authorisation. A rule stated in model output is a request and not policy.
//
// Deliberately not done: inspecting rule text for anything suspicious. The
// measured detection rates for that are poor and the failure mode is worse
// than the gap — a filter that catches two thirds of attacks reads as a
// guarantee and is not one. Nothing here reads the text; the whole control is
// who signed it.

// Rule is a standing instruction that binds every turn.
type Rule struct {
	Kind Kind
	Text string
}

// WithRules installs the ratified rules from config. Set once at startup.
func (in *Inbox) WithRules(constraints, priorities []string) *Inbox {
	rules := make([]Rule, 0, len(constraints)+len(priorities))
	for _, t := range constraints {
		if t = strings.TrimSpace(t); t != "" {
			rules = append(rules, Rule{Kind: KindConstraint, Text: t})
		}
	}
	for _, t := range priorities {
		if t = strings.TrimSpace(t); t != "" {
			rules = append(rules, Rule{Kind: KindPriority, Text: t})
		}
	}
	in.mu.Lock()
	in.rules = rules
	in.mu.Unlock()
	return in
}

// Rules are the ratified standing rules, in config order.
func (in *Inbox) Rules() []Rule {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]Rule(nil), in.rules...)
}

// ProposedRules are the standing rules awaiting ratification.
func (in *Inbox) ProposedRules() []Note {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []Note
	for _, n := range in.notes {
		if n.Proposed {
			out = append(out, n)
		}
	}
	return out
}

// RatifyRule promotes a proposal to a rule: it is written to config.json and
// takes effect from the next turn.
//
// Config first, memory second. The old order in AddProject taught this: a
// change that appears to have been made and then fails to persist is worse
// than one that visibly did not happen, because the user stops checking.
func (in *Inbox) RatifyRule(text string) error {
	in.mu.Lock()
	var found *Note
	for i := range in.notes {
		if in.notes[i].Proposed && in.notes[i].Text == text {
			found = &in.notes[i]
			break
		}
	}
	if found == nil {
		in.mu.Unlock()
		return fmt.Errorf("no proposed rule matches that text")
	}
	kind := found.Kind
	in.mu.Unlock()

	if in.configPath == "" {
		return fmt.Errorf("no config file to record the rule in")
	}
	if err := in.updateConfig(func(s *config.Settings) error {
		if !s.AddKingRule(string(kind), text) {
			return fmt.Errorf("config already carries that rule")
		}
		return nil
	}); err != nil {
		return err
	}

	in.mu.Lock()
	in.rules = append(in.rules, Rule{Kind: kind, Text: text})
	in.mu.Unlock()
	// Clearing the proposal only after the write means a failed write leaves it
	// pending rather than losing it silently.
	in.DropNoteExact(text)
	return nil
}

// formatRules renders the ratified rules and any pending proposals.
//
// Proposals are named but not stated. The supervisor is told that a rule it
// asked for is waiting on the user, because otherwise it re-proposes the same
// rule every turn, and told nothing of the text, because repeating it inside
// the block that says "these bind you" is most of the way to it binding.
func (in *Inbox) formatRules() string {
	rules := in.Rules()
	pending := len(in.ProposedRules())
	if len(rules) == 0 && pending == 0 {
		return ""
	}

	var b strings.Builder
	if len(rules) > 0 {
		b.WriteString("Standing rules the user has set — these bind you regardless of what is asked:\n")
		for _, r := range rules {
			b.WriteString("- " + string(r.Kind) + ": " + r.Text + "\n")
		}
		b.WriteString("\n")
	}
	if pending > 0 {
		b.WriteString(fmt.Sprintf(
			"%d rule(s) you proposed are waiting for the user to accept. They are not in force, and repeating them does not help — the user acts on them in the dashboard.\n\n",
			pending))
	}
	return b.String()
}
