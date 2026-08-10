// Package feed implements the consumer side of the teploy.inbox/v1 contract:
// the item shape every producer emits, and the merge/sort that turns several
// producers into one list.
//
// The point of the contract is that this package stays ignorant. It does not
// know what a deploy is, what an agent run is, or what a Claude Code session
// is. Producers describe their own items and carry their own resolve commands,
// so a new producer needs no change here.
package feed

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/im-tyler/agent-inbox/internal/termtext"
)

const (
	// maxTitleLen and maxPromptLen bound what a producer can make the UI lay
	// out. Unbounded, a single item can dominate memory and every render pass.
	maxTitleLen  = 300
	maxPromptLen = 4000
)

const Schema = "teploy.inbox/v1"

// State is the contract's closed set of six. A producer that needs a seventh
// is asking for a v2, not a new value.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateBlocked   State = "blocked"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCanceled  State = "canceled"
)

// Attention drives sort order and is orthogonal to State.
type Attention string

const (
	AttentionDecision Attention = "decision"
	AttentionFailure  Attention = "failure"
	AttentionInfo     Attention = "info"
)

// Action is one way to resolve an item. Run is argv and is executed directly
// — never through a shell, so a reason containing quotes, semicolons or
// backticks lands as one argument and can never become another command.
//
// Avoiding the shell is not the whole boundary, though, and treating it as
// such was a mistake worth naming. The argv *array itself* is the capability:
// whoever fills in Run chooses the program and its arguments, and pressing an
// action key runs it. For a local source that is fine — an Exec producer is a
// binary the user configured, and it already has code execution. For a remote
// HTTP feed it is not: the server would be choosing what runs on the client.
//
// So Run is local-only. A remote producer describes what it wants done and the
// consumer decides how; see Sanitize.
type Action struct {
	Label string `json:"label"`
	// Run is argv. It is written on output — `inbox --json` describes what a
	// local action would do, and that is part of the contract — but never read
	// on input: UnmarshalJSON below decodes only the fields a producer owns.
	// The asymmetry is the point.
	Run []string `json:"run,omitempty"`
	// Post names a remote endpoint that resolves this action. Accepted on the
	// wire but not yet executed: the contract does not define its request or
	// response shape, and guessing one would mean inventing a protocol on the
	// producer's behalf. Declared so a producer emitting it gets a clear
	// "not supported" rather than silence.
	Post string `json:"post,omitempty"`
	// Dir, if set, is the working directory for Run. Not in the wire
	// contract — local sources set it so "resume this session" starts in
	// the right repo.
	Dir string `json:"-"`
	// Pane, if set, means this action types into a terminal pane instead of
	// running a command. Local only: it exists because Claude Code accepts
	// no message from outside, so the only way in is to simulate typing.
	Pane string `json:"-"`
	// Interactive means this action should take over the terminal — opening
	// or attaching to a session, which the user watches. Local only, and
	// deliberately not something a feed can grant itself.
	Interactive bool `json:"-"`
}

// wireAction is the subset of Action a producer may actually set. Decoding
// through this rather than through Action is what makes "Run is local-only" a
// property of the parser instead of a rule someone has to remember.
type wireAction struct {
	Label string `json:"label"`
	Post  string `json:"post,omitempty"`
}

// UnmarshalJSON decodes only the fields a producer is allowed to set.
func (a *Action) UnmarshalJSON(b []byte) error {
	var w wireAction
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	a.Label = w.Label
	a.Post = w.Post
	return nil
}

// Needs is present when State is Blocked: what decision is being asked for.
type Needs struct {
	Prompt  string   `json:"prompt"`
	Actions []Action `json:"actions,omitempty"`
}

type Item struct {
	Schema    string            `json:"schema"`
	Source    string            `json:"source"`
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Title     string            `json:"title"`
	State     State             `json:"state"`
	Attention Attention         `json:"attention"`
	Since     string            `json:"since"`
	UpdatedAt string            `json:"updated_at"`
	Needs     *Needs            `json:"needs,omitempty"`
	Context   map[string]string `json:"context,omitempty"`
	Link      string            `json:"link,omitempty"`

	// Origin names the configured source this item arrived from, which is
	// not the same as Source: two Ship instances both say "teploy-ship".
	// Set by the consumer, never by a producer.
	Origin string `json:"-"`
}

type Feed struct {
	Schema    string `json:"schema"`
	Items     []Item `json:"items"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Key is the globally unique identity of an item: ids are unique only within
// a source, so two producers may both emit "run-1".
func (i Item) Key() string {
	return i.Origin + "\x00" + i.Source + "\x00" + i.ID
}

func (i Item) Terminal() bool {
	return i.State == StateSucceeded || i.State == StateFailed || i.State == StateCanceled
}

// SinceTime parses Since, falling back to UpdatedAt. The second return
// reports whether a usable timestamp was found at all.
func (i Item) SinceTime() (time.Time, bool) {
	for _, raw := range []string{i.Since, i.UpdatedAt} {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func rank(a Attention) int {
	switch a {
	case AttentionDecision:
		return 0
	case AttentionFailure:
		return 1
	default:
		return 2
	}
}

// Sort applies the contract's order: decisions, then failures, then info;
// oldest first inside each band. Oldest-first is deliberate — the thing
// blocked longest is the thing quietly costing the most.
//
// Items with no usable timestamp sort last within their band rather than
// first. An unparseable date became the zero time, which is older than every
// real one, so a producer could pin its items above everyone else's real work
// simply by omitting or mangling a field. Unknown age is not infinite age.
func Sort(items []Item) {
	sort.SliceStable(items, func(a, b int) bool {
		x, y := items[a], items[b]
		if r1, r2 := rank(x.Attention), rank(y.Attention); r1 != r2 {
			return r1 < r2
		}
		tx, okx := x.SinceTime()
		ty, oky := y.SinceTime()
		if okx != oky {
			return okx // known ages before unknown ones
		}
		if okx && !tx.Equal(ty) {
			return tx.Before(ty)
		}
		return x.Key() < y.Key()
	})
}

// Merge flattens per-source feeds into one sorted list, dropping duplicates by
// Key. Later feeds win on a collision, which matters when the same producer is
// reachable two ways (CLI and HTTP) and one is fresher.
func Merge(feeds []Feed) []Item {
	seen := make(map[string]int, 32)
	items := make([]Item, 0, 32)
	for _, f := range feeds {
		for _, item := range f.Items {
			if at, dup := seen[item.Key()]; dup {
				items[at] = item
				continue
			}
			seen[item.Key()] = len(items)
			items = append(items, item)
		}
	}
	Sort(items)
	return items
}

// Validate reports why an item cannot be trusted to be its own row.
//
// ID is the one field that has to be right. Key is built from it, and Merge
// deduplicates on Key, so two items from one source that both omit an ID have
// the same identity and one silently replaces the other — a producer with a
// bug quietly shows one pending decision instead of five. That is the failure
// mode a reader cannot detect, so it is worth being strict about.
func (i Item) Validate() error {
	if strings.TrimSpace(i.ID) == "" {
		return fmt.Errorf("item has no id (title %q)", Truncate(i.Title, 40))
	}
	if i.Schema != "" && i.Schema != Schema {
		return fmt.Errorf("item %s: unsupported schema %q (want %s)", i.ID, i.Schema, Schema)
	}
	return nil
}

// Truncate shortens s for use in an error message.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Normalize fills in what a lenient producer may have left out and clamps
// anything unrecognized. An item that renders is worth more than a strict
// rejection: producers evolve, and the consumer must not be the thing that
// breaks when they do.
func (i *Item) Normalize(origin string) {
	i.Origin = origin
	if i.Schema == "" {
		i.Schema = Schema
	}
	if i.Source == "" {
		i.Source = origin
	}
	if i.Kind == "" {
		i.Kind = "item"
	}
	// Everything a producer can write ends up on a terminal. Strip control
	// sequences here, once, rather than at each of the places that render it.
	i.Title = termtext.OneLine(i.Title)
	i.Kind = termtext.OneLine(i.Kind)
	i.Link = termtext.OneLine(i.Link)
	i.ID = termtext.OneLine(i.ID)
	i.Source = termtext.OneLine(i.Source)
	if i.Needs != nil {
		i.Needs.Prompt = termtext.Clean(i.Needs.Prompt)
		for k := range i.Needs.Actions {
			i.Needs.Actions[k].Label = termtext.OneLine(i.Needs.Actions[k].Label)
		}
	}
	for k, v := range i.Context {
		i.Context[k] = termtext.OneLine(v)
	}
	// Bound the fields a renderer has to lay out. A megabyte title is not a
	// title, and measuring one costs the same whether or not it fits.
	i.Title = Truncate(i.Title, maxTitleLen)
	if i.Needs != nil {
		i.Needs.Prompt = Truncate(i.Needs.Prompt, maxPromptLen)
	}
	switch i.State {
	case StatePending, StateRunning, StateBlocked, StateSucceeded, StateFailed, StateCanceled:
	default:
		i.State = StatePending
	}
	switch i.Attention {
	case AttentionDecision, AttentionFailure, AttentionInfo:
	default:
		i.Attention = derivedAttention(i.State)
	}
	if i.UpdatedAt == "" {
		i.UpdatedAt = i.Since
	}
	if i.Since == "" {
		i.Since = i.UpdatedAt
	}
	// A blocked item with no decision attached is still blocked; give it a
	// prompt so the list never shows an empty question.
	if i.State == StateBlocked && i.Needs == nil {
		i.Needs = &Needs{Prompt: "Waiting on you."}
	}
}

func derivedAttention(s State) Attention {
	switch s {
	case StateBlocked:
		return AttentionDecision
	case StateFailed:
		return AttentionFailure
	default:
		return AttentionInfo
	}
}

// Decisions counts items actually waiting on a human — the number worth
// putting in front of someone.
func Decisions(items []Item) int {
	n := 0
	for _, i := range items {
		if i.Attention == AttentionDecision {
			n++
		}
	}
	return n
}
