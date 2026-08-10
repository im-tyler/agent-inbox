// Package board is the reader UI: one list of everything waiting on you,
// merged from every configured source.
//
// It deliberately knows nothing about deploys, agent runs or sessions. Items
// describe themselves and carry their own resolve commands, so the only thing
// this package decides is order, layout, and how to run an argv safely.
package board

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/im-tyler/agent-inbox/internal/feed"
	"github.com/im-tyler/agent-inbox/internal/mux"
	"github.com/im-tyler/agent-inbox/internal/sources"
	"github.com/im-tyler/agent-inbox/internal/termtext"
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	mutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	decisionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("221")).Bold(true)
	failureStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	infoStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	selectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("15"))
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

const refreshEvery = 5 * time.Second

// placeholderPattern matches the contract's {token} substitution points.
var placeholderPattern = regexp.MustCompile(`\{([a-z_]+)\}`)

type mode int

const (
	modeList mode = iota
	modeDetail
	modeInput
	modeBroadcast
)

type loadedMsg struct {
	gen     int
	items   []feed.Item
	results []sources.Result
}

type tickMsg time.Time

type ranMsg struct{ err error }

// broadcastMsg reports a finished multi-session send.
type broadcastMsg struct {
	ok       int
	failed   int
	firstErr error
}

type Model struct {
	srcs    []sources.Source
	items   []feed.Item
	results []sources.Result

	cursor int
	mode   mode
	width  int
	height int

	// Pending action awaiting placeholder input.
	pending  feed.Action
	fills    map[string]string
	needed   []string
	input    textinput.Model
	lastErr  error
	loading  bool
	quitting bool

	// gen counts load generations. A completion whose generation is not the
	// current one is discarded: refreshes are started on a timer and can
	// overlap, and without this an older, slower fetch finishing second
	// overwrites the newer state that arrived first.
	gen int

	// detailKey pins the open detail pane to one item's identity. The pane
	// used to resolve through the cursor index, so a refresh that reordered
	// the list changed which item was on screen — and which item an action key
	// then acted on — without the user touching anything.
	detailKey string

	// actionCursor is the highlighted action inside the detail pane.
	actionCursor int

	// broadcastTargets is the set of item keys a composing broadcast will go
	// to, captured when composition began.
	broadcastTargets []string

	// showAll includes everything that is merely happening. Off by default:
	// an inbox of twelve sessions that want nothing is the pane-switching
	// problem again, wearing a list.
	showAll bool

	// marked holds items selected for a broadcast, keyed by Item.Key().
	marked map[string]bool
	// sent reports the outcome of the last broadcast.
	sent string
	// showHelp expands the key list.
	showHelp bool

	// embedded means the board is hosted inside the supervisor rather than
	// run as its own program. The host owns leaving and adopting, so the
	// board only has to describe those keys, not implement them.
	embedded bool
}

func New(srcs []sources.Source) Model {
	in := textinput.New()
	in.Prompt = "> "
	in.CharLimit = 500
	return Model{srcs: srcs, input: in, loading: true, marked: map[string]bool{}}
}

// SetShowAll starts the board with everything visible.
func (m Model) SetShowAll(all bool) Model { m.showAll = all; return m }

// SetEmbedded marks the board as hosted inside the supervisor.
func (m Model) SetEmbedded(b bool) Model { m.embedded = b; return m }

// Selected is the row under the cursor, for a host that wants to act on it.
func (m Model) Selected() (feed.Item, bool) { return m.selected() }

// Reload refetches every source. The host calls this after a change it knows
// the sources cannot see yet.
func (m Model) Reload() (Model, tea.Cmd) {
	m.loading = true
	return m, m.load()
}

// visible is what the list renders: only what wants something from you,
// unless you ask for the rest.
func (m Model) visible() []feed.Item {
	if m.showAll {
		return m.items
	}
	out := make([]feed.Item, 0, len(m.items))
	for _, item := range m.items {
		if item.Attention != feed.AttentionInfo {
			out = append(out, item)
		}
	}
	return out
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.load(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// load starts a fetch tagged with the generation it belongs to. Callers bump
// m.gen first; the completion handler compares and discards stale arrivals.
func (m Model) load() tea.Cmd {
	srcs, gen := m.srcs, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		items, results := sources.FetchAll(ctx, srcs)
		return loadedMsg{gen: gen, items: items, results: results}
	}
}

// startLoad bumps the generation and returns the command to run it.
func (m *Model) startLoad() tea.Cmd {
	m.gen++
	m.loading = true
	return m.load()
}

func (m Model) selected() (feed.Item, bool) {
	// In the detail pane the pinned key wins, so a reorder underneath does not
	// change what is on screen or what an action applies to.
	if m.mode == modeDetail && m.detailKey != "" {
		return m.byKey(m.detailKey)
	}
	items := m.visible()
	if m.cursor < 0 || m.cursor >= len(items) {
		return feed.Item{}, false
	}
	return items[m.cursor], true
}

// byKey finds an item by its stable identity.
func (m Model) byKey(key string) (feed.Item, bool) {
	for _, item := range m.items {
		if item.Key() == key {
			return item, true
		}
	}
	return feed.Item{}, false
}

// run executes an action's argv directly — no shell is involved, so a
// substituted reason containing quotes, semicolons or backticks lands as one
// argument and can never become another command.
//
// Whether it takes over the terminal depends on the action. Opening or
// attaching to a session is something the user watches; a reply or an approval
// is a request that may run a full model turn, and suspending the whole inbox
// for minutes to wait for one is not the same thing at all.
func run(action feed.Action, fills map[string]string) tea.Cmd {
	// A remote action declares an endpoint rather than a command. The contract
	// defines the field but not its request or response shape, so there is
	// nothing to implement against yet — and translating it into a curl argv
	// would be inventing a protocol on the producer's behalf. Say so plainly
	// rather than reporting the generic "no command".
	if len(action.Run) == 0 && action.Pane == "" && action.Post != "" {
		return func() tea.Msg {
			return ranMsg{err: fmt.Errorf("action %q is a remote action; this build does not execute those", action.Label)}
		}
	}
	argv := substitute(action.Run, fills)
	if len(argv) == 0 {
		return func() tea.Msg { return ranMsg{err: fmt.Errorf("action %q has no command", action.Label)} }
	}
	// A pane action types into a terminal rather than running a command.
	// It stays in the background: unlike attaching, there is nothing for the
	// user to look at, and taking over the screen to type one line would be
	// worse than not.
	if action.Pane != "" {
		text := strings.Join(argv, " ")
		return func() tea.Msg {
			m := mux.Detect()
			if m == nil {
				return ranMsg{err: fmt.Errorf("no zellij or tmux session to type into")}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return ranMsg{err: m.Send(ctx, action.Pane, text)}
		}
	}
	if !action.Interactive {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Dir = action.Dir
			out, err := cmd.CombinedOutput()
			if err != nil && len(out) > 0 {
				err = fmt.Errorf("%w: %s", err, termtext.OneLine(string(out)))
			}
			return ranMsg{err: err}
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = action.Dir
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return ranMsg{err: err} })
}

// actionTimeout bounds a background action. A reply is a model turn, so it is
// generous; it exists to stop a hung CLI holding a slot forever.
const actionTimeout = 15 * time.Minute

// substitute replaces {token} occurrences with the value collected for that
// token. A placeholder is always a whole argv element after substitution,
// never a shell fragment.
//
// Substitution is by name. Filling positionally advanced one value per
// *argument* but replaced every occurrence within it, so an argv element like
// "--pair={user}:{reason}" prompted for two values and then wrote the first
// one into both slots. Repeating {message} twice now reuses one value, which
// is also what a reader of the contract would expect.
func substitute(argv []string, fills map[string]string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		if !placeholderPattern.MatchString(arg) {
			out = append(out, arg)
			continue
		}
		replaced := placeholderPattern.ReplaceAllStringFunc(arg, func(tok string) string {
			name := strings.Trim(tok, "{}")
			return fills[name]
		})
		// An empty optional field (a denial with no reason given) should
		// drop the argument rather than pass "".
		if strings.TrimSpace(replaced) == "" {
			continue
		}
		out = append(out, replaced)
	}
	return out
}

// placeholders lists the distinct tokens an action needs filled, in the order
// they are first seen.
func placeholders(action feed.Action) []string {
	var out []string
	seen := map[string]bool{}
	for _, arg := range action.Run {
		for _, match := range placeholderPattern.FindAllStringSubmatch(arg, -1) {
			if !seen[match[1]] {
				seen[match[1]] = true
				out = append(out, match[1])
			}
		}
	}
	return out
}

func (m Model) actionsFor(item feed.Item) []feed.Action {
	if item.Needs == nil {
		return nil
	}
	return item.Needs.Actions
}

// sendable returns the action that delivers a message to this item, if it has
// one. opencode and codex accept a prompt into an existing session; a Claude
// Code session can only be typed into, and only when its pane resolved
// unambiguously. Items with neither are skipped by a broadcast rather than
// silently counted as delivered.
func sendable(item feed.Item) (feed.Action, bool) {
	if item.Needs == nil {
		return feed.Action{}, false
	}
	for _, a := range item.Needs.Actions {
		if a.Label == "reply" || a.Label == "send" {
			return a, true
		}
	}
	return feed.Action{}, false
}

// broadcast delivers text to the targets captured when composition began,
// concurrently. Each send is a model turn that can run for minutes, so doing
// them in sequence would block the UI for as long as the slowest one.
//
// The targets are a snapshot rather than a fresh scan of the marks. Marks
// persist across the `a` filter, so recomputing at send time could deliver to
// a session the user had marked and then hidden — a recipient not on screen
// when they pressed enter.
func (m Model) broadcast(text string) tea.Cmd {
	var targets []feed.Action
	for _, key := range m.broadcastTargets {
		item, ok := m.byKey(key)
		if !ok {
			continue
		}
		if a, ok := sendable(item); ok {
			targets = append(targets, a)
		}
	}
	return func() tea.Msg {
		type result struct{ err error }
		results := make(chan result, len(targets))
		for _, a := range targets {
			go func(a feed.Action) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				results <- result{err: deliver(ctx, a, text)}
			}(a)
		}
		msg := broadcastMsg{}
		for range targets {
			if r := <-results; r.err != nil {
				msg.failed++
				if msg.firstErr == nil {
					msg.firstErr = r.err
				}
			} else {
				msg.ok++
			}
		}
		return msg
	}
}

// deliver performs one send, by typing into a pane or by running the action's
// command. Unlike the interactive path this never takes over the terminal —
// a broadcast is fire-and-report.
func deliver(ctx context.Context, action feed.Action, text string) error {
	if action.Pane != "" {
		m := mux.Detect()
		if m == nil {
			return fmt.Errorf("no zellij or tmux session to type into")
		}
		return m.Send(ctx, action.Pane, text)
	}
	// Every placeholder gets the broadcast text: a send action's argv has one
	// message slot, whatever it is called.
	fills := map[string]string{}
	for _, name := range placeholders(action) {
		fills[name] = text
	}
	argv := substitute(action.Run, fills)
	if len(argv) == 0 {
		return fmt.Errorf("action %q has no command", action.Label)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = action.Dir
	return cmd.Run()
}

// visibleMarkedKeys are the marked items currently on screen, in list order.
func (m Model) visibleMarkedKeys() []string {
	var out []string
	for _, item := range m.visible() {
		if m.marked[item.Key()] {
			out = append(out, item.Key())
		}
	}
	return out
}

// markedSendable counts visible marked items that can actually receive a
// message. Marking a Claude Code session whose pane did not resolve is allowed
// — it just cannot be part of a broadcast, and saying so beats a silent no-op.
func (m Model) markedSendable() int {
	n := 0
	for _, key := range m.visibleMarkedKeys() {
		item, ok := m.byKey(key)
		if !ok {
			continue
		}
		if _, ok := sendable(item); ok {
			n++
		}
	}
	return n
}

// hiddenMarked counts marks on rows the current filter is not showing, so a
// delivery count smaller than the number of marks is explainable.
func (m Model) hiddenMarked() int {
	visible := map[string]bool{}
	for _, item := range m.visible() {
		visible[item.Key()] = true
	}
	n := 0
	for key := range m.marked {
		if !visible[key] {
			n++
		}
	}
	return n
}

func (m Model) startAction(action feed.Action) (Model, tea.Cmd) {
	needed := placeholders(action)
	if len(needed) == 0 {
		return m, run(action, nil)
	}
	m.pending = action
	m.fills = map[string]string{}
	m.needed = needed
	m.mode = modeInput
	m.input.SetValue("")
	m.input.Placeholder = needed[0]
	m.input.Focus()
	return m, textinput.Blink
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case loadedMsg:
		// Discard a completion from a superseded generation. Two refreshes can
		// be in flight at once, and the older one finishing last would put
		// stale rows back on screen.
		if msg.gen != m.gen {
			return m, nil
		}
		// Keep the highlight on the same item across a reorder where possible.
		var focused string
		if item, ok := m.selected(); ok {
			focused = item.Key()
		}
		m.items, m.results, m.loading = msg.items, msg.results, false
		m.restoreCursor(focused)
		return m, nil

	case tickMsg:
		// Never refetch under an open prompt: the list reordering beneath a
		// half-typed decision is how you approve the wrong thing.
		if m.mode == modeInput || m.mode == modeBroadcast {
			return m, tick()
		}
		// One at a time. An automatic tick that starts a load while one is
		// already running just adds concurrent work whose results have to be
		// thrown away.
		if m.loading {
			return m, tick()
		}
		return m, tea.Batch(m.startLoad(), tick())

	case ranMsg:
		m.lastErr = msg.err
		m.mode = modeList
		m.detailKey = ""
		return m, m.startLoad()

	case broadcastMsg:
		m.sent = fmt.Sprintf("sent to %d", msg.ok)
		if msg.failed > 0 {
			m.sent = fmt.Sprintf("sent to %d, %d failed", msg.ok, msg.failed)
			m.lastErr = msg.firstErr
		}
		m.marked = map[string]bool{}
		m.broadcastTargets = nil
		return m, m.startLoad()

	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

// restoreCursor puts the highlight back on the item it was on, or as close as
// the new list allows.
func (m *Model) restoreCursor(key string) {
	items := m.visible()
	if key != "" {
		for i, item := range items {
			if item.Key() == key {
				m.cursor = i
				return
			}
		}
	}
	if m.cursor >= len(items) {
		m.cursor = max(0, len(items)-1)
	}
}

// CapturingInput reports whether the board is collecting text, so a host does
// not steal ordinary characters from it.
func (m Model) CapturingInput() bool {
	return m.mode == modeInput || m.mode == modeBroadcast
}

// HostIntent is what the board asks its host to do with a key it did not
// consume itself.
type HostIntent int

const (
	// HostNoIntent means the board handled the key.
	HostNoIntent HostIntent = iota
	// HostLeave means the user asked to leave the inbox view.
	HostLeave
	// HostAdopt means the user asked to adopt the selected row.
	HostAdopt
)

// EmbeddedKey routes a key while the board is hosted inside another program.
//
// The board decides, because only it knows whether it is currently collecting
// text. The host used to guess: it claimed 'n', 'q' and Esc before the board
// saw them, which meant that typing "no" into an action prompt adopted a
// project, typing "queue it" left the view, and Esc abandoned the whole inbox
// instead of cancelling the prompt. A side effect as large as adopting a
// project should not be reachable by typing a letter into a text field.
func (m Model) EmbeddedKey(msg tea.KeyMsg) (Model, tea.Cmd, HostIntent) {
	if m.CapturingInput() {
		updated, cmd := m.Update(msg)
		return updated.(Model), cmd, HostNoIntent
	}
	switch msg.String() {
	case "esc":
		if m.mode == modeDetail {
			updated, cmd := m.Update(msg)
			return updated.(Model), cmd, HostNoIntent
		}
		return m, nil, HostLeave
	case "q":
		return m, nil, HostLeave
	case "n":
		return m, nil, HostAdopt
	}
	updated, cmd := m.Update(msg)
	return updated.(Model), cmd, HostNoIntent
}

// SetSize gives the board its dimensions. A hosted board never receives a
// WindowSizeMsg of its own until the terminal is next resized, so without this
// it renders its first frame at the 100-column fallback regardless of how wide
// the terminal actually is.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	m.input.Width = max(20, width-8)
	return m
}

func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mode == modeBroadcast {
		switch msg.Type {
		case tea.KeyEsc:
			m.mode = modeList
			m.broadcastTargets = nil
			return m, nil
		case tea.KeyEnter:
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				m.mode = modeList
				m.broadcastTargets = nil
				return m, nil
			}
			m.mode = modeList
			m.sent = "sending…"
			return m, m.broadcast(text)
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	if m.mode == modeInput {
		switch msg.Type {
		case tea.KeyEsc:
			m.mode = modeList
			return m, nil
		case tea.KeyEnter:
			m.fills[m.needed[len(m.fills)]] = m.input.Value()
			if len(m.fills) < len(m.needed) {
				m.input.SetValue("")
				m.input.Placeholder = m.needed[len(m.fills)]
				return m, nil
			}
			action, fills := m.pending, m.fills
			m.mode = modeList
			return m, run(action, fills)
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// In the detail pane the action list has its own cursor, so an item with
	// more than five actions is fully reachable. Numbers still work as
	// shortcuts for the first nine.
	if m.mode == modeDetail {
		switch msg.String() {
		case "j", "down":
			if item, ok := m.selected(); ok && m.actionCursor < len(m.actionsFor(item))-1 {
				m.actionCursor++
			}
			return m, nil
		case "k", "up":
			if m.actionCursor > 0 {
				m.actionCursor--
			}
			return m, nil
		case "enter":
			item, ok := m.selected()
			if !ok {
				m.mode = modeList
				m.detailKey = ""
				return m, nil
			}
			actions := m.actionsFor(item)
			if m.actionCursor < len(actions) {
				return m.startAction(actions[m.actionCursor])
			}
			m.mode = modeList
			m.detailKey = ""
			return m, nil
		case "esc":
			m.mode = modeList
			m.detailKey = ""
			return m, nil
		}
	}

	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.showHelp = !m.showHelp
	case "a":
		m.showAll = !m.showAll
		m.cursor = 0
	case " ":
		if item, ok := m.selected(); ok {
			if m.marked[item.Key()] {
				delete(m.marked, item.Key())
			} else {
				m.marked[item.Key()] = true
			}
		}
	case "b":
		if m.markedSendable() == 0 {
			m.lastErr = fmt.Errorf("nothing marked that can receive a message (space to mark)")
			break
		}
		// Capture the recipients now, from what is on screen now.
		m.broadcastTargets = m.visibleMarkedKeys()
		m.mode = modeBroadcast
		m.sent = ""
		m.input.SetValue("")
		m.input.Placeholder = "message to every marked session"
		m.input.Focus()
		return m, textinput.Blink
	case "j", "down":
		if m.cursor < len(m.visible())-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = max(0, len(m.visible())-1)
	case "r":
		if m.loading {
			break
		}
		return m, m.startLoad()
	case "enter":
		if item, ok := m.selected(); ok {
			m.mode = modeDetail
			m.detailKey = item.Key()
			m.actionCursor = 0
		}
	case "esc":
		m.mode = modeList
		m.detailKey = ""
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		item, ok := m.selected()
		if !ok {
			break
		}
		actions := m.actionsFor(item)
		idx := int(msg.String()[0] - '1')
		if idx < len(actions) {
			return m.startAction(actions[idx])
		}
	}
	return m, nil
}

// genericAsks are the placeholder prompts a source emits when it has nothing
// real to report. They stay in the feed because the contract wants a prompt on
// every blocked item, but repeating "Finished its turn — waiting on you." down
// five rows is noise wearing the shape of information.
var genericAsks = map[string]bool{
	"finished its turn — waiting on you.":                  true,
	"background agent finished its turn — waiting on you.": true,
	"this run is parked waiting for approval.":             true,
	"waiting on you.": true,
}

func realAsk(item feed.Item) string {
	if item.Needs == nil {
		return ""
	}
	ask := strings.TrimSpace(item.Needs.Prompt)
	if ask == "" || genericAsks[strings.ToLower(ask)] {
		return ""
	}
	return ask
}

// project is the orienting fact — which repo is this? — so it earns a column
// rather than being buried in the detail pane.
func project(item feed.Item) string {
	if p := item.Context["project"]; p != "" {
		return p
	}
	return item.Origin
}

func tag(a feed.Attention) string {
	switch a {
	case feed.AttentionDecision:
		return decisionStyle.Render("decision")
	case feed.AttentionFailure:
		return failureStyle.Render("failure ")
	default:
		return infoStyle.Render("info    ")
	}
}

func age(item feed.Item) string {
	t, ok := item.SinceTime()
	if !ok {
		// Unknown, not ancient. Showing nothing here read as "just arrived".
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		// "just now" is eight characters in a five-wide column and shunts
		// every following column out of line.
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// clip shortens s to n terminal cells.
//
// A non-positive width now yields the empty string. It used to return the
// whole string, which inverted the intent exactly where it mattered: the
// column widths are computed by subtraction, so the narrower the terminal, the
// more likely a width went non-positive — and the more likely clipping
// switched itself off and the row overflowed.
func clip(s string, n int) string {
	return termtext.Truncate(termtext.OneLine(s), n)
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder

	decisions := feed.Decisions(m.items)
	head := fmt.Sprintf("agent inbox — %d waiting on you", decisions)
	if decisions == 0 {
		head = "agent inbox — nothing waiting on you"
	}
	b.WriteString(titleStyle.Render(head) + "\n")
	if m.loading && len(m.items) == 0 {
		b.WriteString(mutedStyle.Render("loading…") + "\n")
	}
	for _, bad := range sources.Errors(m.results) {
		b.WriteString(errStyle.Render("! "+clip(bad.Err.Error(), maxWidth(m.width))) + "\n")
	}
	if m.lastErr != nil {
		b.WriteString(errStyle.Render("! "+clip(m.lastErr.Error(), maxWidth(m.width))) + "\n")
	}
	b.WriteString("\n")

	items := m.visible()
	if len(items) == 0 && !m.loading {
		if len(m.items) > 0 {
			b.WriteString(mutedStyle.Render(fmt.Sprintf("  nothing needs you — %d running (a to show)\n", len(m.items))))
		} else {
			b.WriteString(mutedStyle.Render("  inbox empty\n"))
		}
	}

	width := maxWidth(m.width)

	// The attention column only earns its space when the list is mixed. In
	// the default view every row is a decision, so the word repeats down the
	// screen carrying nothing.
	mixed := false
	for _, item := range items {
		if item.Attention != items[0].Attention {
			mixed = true
			break
		}
	}

	projWidth := 0
	for _, item := range items {
		if n := len([]rune(project(item))); n > projWidth {
			projWidth = n
		}
	}
	if projWidth > 18 {
		projWidth = 18
	}

	for i, item := range items {
		// Cell-aware padding, not %-*s: that pads by rune count, so one CJK
		// title shifts every column after it by the number of wide glyphs.
		titleWidth := width - projWidth - 24
		row := termtext.Pad(age(item), 5) + " " + termtext.Pad(project(item), projWidth) +
			" " + clip(item.Title, titleWidth)
		if mixed {
			row = tag(item.Attention) + "  " + row
		}

		mark := " "
		if m.marked[item.Key()] {
			if _, ok := sendable(item); ok {
				mark = "*"
			} else {
				// Marked but unreachable: shown so the delivery count being
				// lower than the number of marks is not a mystery.
				mark = "-"
			}
		}
		if i == m.cursor {
			b.WriteString(selectedStyle.Render(mark+"> "+row) + "\n")
		} else {
			b.WriteString(mark + "  " + row + "\n")
		}
		// Only a real question earns a second line.
		if ask := realAsk(item); ask != "" {
			b.WriteString(mutedStyle.Render("     "+clip(ask, width-8)) + "\n")
		}
	}

	if m.mode == modeDetail {
		b.WriteString(m.detail())
	}
	if m.mode == modeInput {
		b.WriteString("\n" + titleStyle.Render(m.pending.Label) + " — " + mutedStyle.Render(m.input.Placeholder) + "\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString(mutedStyle.Render("enter to confirm · esc to cancel") + "\n")
		return b.String()
	}
	if m.mode == modeBroadcast {
		b.WriteString("\n" + titleStyle.Render(fmt.Sprintf("broadcast to %d session(s)", m.markedSendable())) + "\n")
		b.WriteString(m.input.View() + "\n")
		b.WriteString(mutedStyle.Render("enter to send · esc to cancel") + "\n")
		return b.String()
	}

	if m.sent != "" {
		b.WriteString("\n" + titleStyle.Render(m.sent) + "\n")
	}
	// Seven keys on one line reads as clutter and gets skipped. Show the
	// three that matter and put the rest behind ?.
	hidden := len(m.items) - len(items)
	var help string
	switch {
	case m.showHelp && m.embedded:
		help = "j/k move · enter detail · n add as project · space mark · b broadcast · 1-5 act · a show/hide running · r refresh · ? less · esc back"
	case m.showHelp:
		help = "j/k move · enter detail · space mark · b broadcast · 1-5 act · a show/hide running · r refresh · ? less · q quit"
	case m.embedded && hidden > 0:
		help = fmt.Sprintf("n add as project · enter detail · a show %d running · ? keys", hidden)
	case m.embedded:
		help = "n add as project · enter detail · ? keys"
	case hidden > 0:
		help = fmt.Sprintf("enter detail · b broadcast · a show %d running · ? keys", hidden)
	default:
		help = "enter detail · b broadcast · ? keys"
	}
	b.WriteString("\n" + mutedStyle.Render(help) + "\n")
	return b.String()
}

func (m Model) detail() string {
	item, ok := m.selected()
	if !ok {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n" + titleStyle.Render(item.Title) + "\n")
	b.WriteString(mutedStyle.Render(fmt.Sprintf("%s · %s · %s · %s", item.Origin, item.Kind, item.State, item.ID)) + "\n")
	if item.Needs != nil && item.Needs.Prompt != "" {
		b.WriteString("\n" + item.Needs.Prompt + "\n")
	}
	if len(item.Context) > 0 {
		b.WriteString("\n")
		for _, k := range sortedKeys(item.Context) {
			b.WriteString(mutedStyle.Render(fmt.Sprintf("  %-12s %s", k, clip(item.Context[k], maxWidth(m.width)-16))) + "\n")
		}
	}
	if item.Link != "" {
		b.WriteString(mutedStyle.Render("  link         "+item.Link) + "\n")
	}
	actions := m.actionsFor(item)
	if len(actions) > 0 {
		b.WriteString("\n")
		for i, a := range actions {
			// Every action shown is reachable: the first nine by number, all
			// of them by j/k and enter. Rendering a numbered list longer than
			// the keys that select it advertised actions nobody could run.
			label := fmt.Sprintf("  [%d] %s", i+1, a.Label)
			if i >= 9 {
				label = "      " + a.Label
			}
			detail := strings.Join(a.Run, " ")
			if detail == "" && a.Post != "" {
				detail = "remote action (not supported)"
			}
			line := label + " " + mutedStyle.Render(clip(detail, max(10, maxWidth(m.width)-len(label)-2)))
			if i == m.actionCursor {
				line = selectedStyle.Render(clip(label+" "+detail, maxWidth(m.width)))
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func maxWidth(w int) int {
	if w <= 0 {
		return 100
	}
	return w
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Run starts the board.
func Run(srcs []sources.Source, showAll bool) error {
	_, err := tea.NewProgram(New(srcs).SetShowAll(showAll), tea.WithAltScreen()).Run()
	return err
}

// InDetail reports whether the board has a detail pane open, so a host can
// tell "close the detail" from "leave the inbox".
func (m Model) InDetail() bool { return m.mode == modeDetail }
