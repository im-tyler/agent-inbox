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
	"sort"
	"strings"
	"time"
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
// — never through a shell. That is the whole security boundary: a reason
// containing quotes, semicolons or backticks lands as one argument and can
// never become another command.
type Action struct {
	Label string   `json:"label"`
	Run   []string `json:"run,omitempty"`
	Post  string   `json:"post,omitempty"`
	// Dir, if set, is the working directory for Run. Not in the wire
	// contract — local sources set it so "resume this session" starts in
	// the right repo.
	Dir string `json:"-"`
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

// SinceTime parses Since, falling back to UpdatedAt and then to the zero time.
// A producer with a malformed timestamp sorts to the top rather than being
// dropped — better to show something odd than to hide it.
func (i Item) SinceTime() time.Time {
	for _, raw := range []string{i.Since, i.UpdatedAt} {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t
		}
	}
	return time.Time{}
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
func Sort(items []Item) {
	sort.SliceStable(items, func(a, b int) bool {
		x, y := items[a], items[b]
		if r1, r2 := rank(x.Attention), rank(y.Attention); r1 != r2 {
			return r1 < r2
		}
		tx, ty := x.SinceTime(), y.SinceTime()
		if !tx.Equal(ty) {
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
	i.Title = strings.Join(strings.Fields(i.Title), " ")
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
