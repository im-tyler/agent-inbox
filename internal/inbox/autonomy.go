package inbox

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/ident"
)

// Waking the supervisor without being asked.
//
// Everything else in this program happens because the user typed something.
// This does not, and that is the whole risk: an autonomous supervisor spends
// real money while nobody is watching, and one making mediocre decisions
// unattended is worse than no supervisor at all.
//
// So the guardrails are not a later hardening pass, they are the feature:
//
//   - Off unless switched on. king.autonomous defaults to false.
//   - A wake budget per hour, separate from the round budget, clamped.
//   - Never while that supervisor is already working, and never while the user
//     has an unsent draft — they are mid-thought, and a turn starting under
//     them changes the fleet their message was about.
//   - Coalesced. Three projects finishing within a few seconds is one wake.
//   - Every wake writes a line into the supervisor's own thread naming what
//     woke it. A supervisor that acted overnight and cannot say why is not
//     auditable, and an unauditable one cannot be trusted with the next
//     increment of authority.

const (
	// DefaultWakesPerHour is the budget when config does not set one. Four is
	// a supervisor that notices things, not one that hovers.
	DefaultWakesPerHour = 4
	// maxWakesPerHour caps what config may ask for. A budget large enough to
	// be indistinguishable from no budget would not be one.
	maxWakesPerHour = 30
	// coalesceWindow is how long to wait after a trigger before waking, so that
	// a fleet finishing together produces one turn rather than one each.
	coalesceWindow = 5 * time.Second
)

// Overseer wakes a group's supervisor when something in its fleet changes.
type Overseer struct {
	in *Inbox

	mu sync.Mutex
	// wakes is when each wake happened, oldest first, trimmed to the last hour.
	wakes []time.Time
	// pending holds the triggers gathered since the last wake.
	pending map[string]string
	timer   *time.Timer

	perHour  int
	draftFn  func() bool
	nowFn    func() time.Time
	coalesce time.Duration
}

// WithAutonomy switches the supervisor from answering to noticing.
//
// perHour is the wake budget; zero selects the default and values above the cap
// are clamped. draft reports whether the user is mid-message — the inbox cannot
// know that, the UI can, so it is injected.
//
// Returns nil when autonomy is off, and every method tolerates a nil receiver,
// so the caller has nothing to branch on.
func (in *Inbox) WithAutonomy(enabled bool, perHour int, draft func() bool) *Overseer {
	if !enabled {
		return nil
	}
	switch {
	case perHour <= 0:
		perHour = DefaultWakesPerHour
	case perHour > maxWakesPerHour:
		perHour = maxWakesPerHour
	}
	if draft == nil {
		draft = func() bool { return false }
	}
	o := &Overseer{
		in:       in,
		pending:  map[string]string{},
		perHour:  perHour,
		draftFn:  draft,
		coalesce: coalesceWindow,
	}
	in.mu.Lock()
	in.overseer = o
	in.mu.Unlock()
	return o
}

// oversight is the configured Overseer, or nil. Read under the mutex because
// Ingest calls it from the poll loop while startup may still be assembling the
// inbox.
func (in *Inbox) oversight() *Overseer {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.overseer
}

// SetDraft records whether the user has an unsent message in the composer.
//
// The inbox cannot know this and the UI can, so the UI tells it. It lives here
// rather than in a closure over the Bubble Tea model because that model is a
// value copied on every update, and a closure over one would report the draft
// state of whichever copy it happened to capture.
func (in *Inbox) SetDraft(has bool) {
	in.mu.Lock()
	in.draft = has
	in.mu.Unlock()
}

// HasDraft reports an unsent composer message. The overseer refuses to wake
// while one exists: the user is mid-thought, and a turn starting under them
// changes the fleet their message was about between writing it and sending it.
func (in *Inbox) HasDraft() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.draft
}

func (o *Overseer) now() time.Time {
	if o.nowFn != nil {
		return o.nowFn()
	}
	return time.Now()
}

// Notice records that a project changed state, and schedules a wake.
//
// It does not wake immediately. A fleet that finishes together should produce
// one supervisor turn that sees all of it, not three that each see a slice and
// dispatch against a fleet still in motion.
func (o *Overseer) Notice(project, why string) {
	if o == nil || project == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pending[ident.Name(project)] = why
	if o.timer != nil {
		return // a wake is already scheduled; this joins it
	}
	o.timer = time.AfterFunc(o.coalesce, func() {
		o.mu.Lock()
		o.timer = nil
		o.mu.Unlock()
		// Through track, so Close waits for it. AfterFunc runs on a goroutine
		// of its own that nothing else knows about, and a wake that fired as
		// the program was quitting went on writing state — and stderr warnings
		// about a state file whose directory had already gone — after Close had
		// promised there was nothing left running.
		o.in.track(o.wake)
	})
}

// Stop cancels a scheduled wake. Called from shutdown, so a timer that has
// already fired does not start a turn into an inbox that is closing.
func (o *Overseer) Stop() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.timer != nil {
		o.timer.Stop()
		o.timer = nil
	}
	o.pending = map[string]string{}
}

// wake starts one autonomous turn per group with something pending.
func (o *Overseer) wake() {
	if o == nil || o.in.closed() {
		return
	}
	o.mu.Lock()
	triggers := o.pending
	o.pending = map[string]string{}
	o.mu.Unlock()
	if len(triggers) == 0 {
		return
	}

	// The user being mid-message outranks everything. Starting a turn under
	// them changes the fleet their message was about between writing it and
	// sending it.
	if o.draftFn() {
		return
	}

	groups := o.in.Groups()
	for gi := range groups {
		fleet := o.in.FleetNamesOf(gi)
		mine := map[string]string{}
		for _, name := range fleet {
			if why, ok := triggers[ident.Name(name)]; ok {
				mine[name] = why
			}
		}
		if len(mine) == 0 {
			continue
		}
		o.wakeGroup(gi, mine)
	}
}

func (o *Overseer) wakeGroup(gi int, triggers map[string]string) {
	kingIdx := o.in.KingIndexOf(gi)
	kingName := o.in.KingNameOf(gi)
	if kingIdx == 0 || kingName == "" {
		return
	}
	// A supervisor mid-turn is already looking at this fleet. Waking it would
	// be refused as "already working" and the trigger would be lost saying so.
	for _, p := range o.in.Snapshot() {
		if p.Name == kingName && p.Status == driver.StatusWorking {
			return
		}
	}
	if !o.spendWake() {
		o.in.noteToKing(kingName, fmt.Sprintf(
			"woke on %s but this hour's wake budget is spent (king.wakes_per_hour = %d) — nothing was dispatched",
			strings.Join(sortedNames(triggers), ", "), o.perHour))
		return
	}

	// Say what woke it, in its own thread, before it says anything. This is the
	// only record that the turn happened at all, and the only way to audit a
	// night of them afterwards.
	o.in.noteToKing(kingName, "woke: "+describeTriggers(triggers))

	prompt := autonomousPrompt(triggers)
	if err := o.in.KingSend(kingIdx, prompt, o.in.FleetNamesOf(gi)); err != nil {
		o.in.noteToKing(kingName, "woke but could not start a turn: "+err.Error())
	}
}

// spendWake takes one from this hour's budget, reporting false when it is gone.
func (o *Overseer) spendWake() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	cutoff := o.now().Add(-time.Hour)
	kept := o.wakes[:0]
	for _, w := range o.wakes {
		if w.After(cutoff) {
			kept = append(kept, w)
		}
	}
	o.wakes = kept
	if len(o.wakes) >= o.perHour {
		return false
	}
	o.wakes = append(o.wakes, o.now())
	return true
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Deterministic, so two identical wakes read identically in the thread.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func describeTriggers(triggers map[string]string) string {
	parts := make([]string, 0, len(triggers))
	for _, name := range sortedNames(triggers) {
		if why := triggers[name]; why != "" {
			parts = append(parts, name+" ("+why+")")
			continue
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ", ")
}

// autonomousPrompt is the turn the supervisor gets when nobody asked it
// anything.
//
// It is a different shape from a user message on purpose. There is no question
// to answer, so an open-ended prompt produces a paragraph nobody reads; and
// there is nobody watching, so an open-ended set of possible actions is an
// unbounded licence. The decision comes from a closed vocabulary, and only one
// of the four produces text for the user.
func autonomousPrompt(triggers map[string]string) string {
	var b strings.Builder
	b.WriteString("No one has asked you anything. You were woken because the fleet changed:\n")
	for _, name := range sortedNames(triggers) {
		b.WriteString("- " + name)
		if why := triggers[name]; why != "" {
			b.WriteString(": " + why)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nDecide what to do, and pick exactly one of these four:\n\n")
	b.WriteString("UNBLOCK — a project is stuck on something you can resolve by asking another project.\n")
	b.WriteString("  Emit the [send to ...] directives and say in one line what you are unblocking.\n")
	b.WriteString("REPROMPT — a project stopped short of what was asked and can continue on its own.\n")
	b.WriteString("  Emit one [send to ...] and say in one line why.\n")
	b.WriteString("PARK — nothing needs doing. Reply with the single word PARK and nothing else.\n")
	b.WriteString("ESCALATE — this needs the user. Say what happened and what you need, in two lines.\n\n")
	b.WriteString("Default to PARK. You are spending the user's money unattended, and a turn that")
	b.WriteString(" was not needed costs more than a delay. Escalate only for something that")
	b.WriteString(" genuinely cannot proceed without a decision only they can make.\n")
	b.WriteString("Record anything durable you learned with [note: ...].\n")
	return b.String()
}
