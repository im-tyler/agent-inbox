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
	fills    []string
	input    textinput.Model
	lastErr  error
	loading  bool
	quitting bool

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

func (m Model) load() tea.Cmd {
	srcs := m.srcs
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		items, results := sources.FetchAll(ctx, srcs)
		return loadedMsg{items: items, results: results}
	}
}

func (m Model) selected() (feed.Item, bool) {
	items := m.visible()
	if m.cursor < 0 || m.cursor >= len(items) {
		return feed.Item{}, false
	}
	return items[m.cursor], true
}

// run executes an action's argv directly — no shell is involved, so a
// substituted reason containing quotes, semicolons or backticks lands as one
// argument and can never become another command.
func run(action feed.Action, fills []string) tea.Cmd {
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
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = action.Dir
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return ranMsg{err: err} })
}

// substitute replaces {token} occurrences in order, one fill per placeholder.
// A placeholder is always a whole argv element after substitution, never a
// shell fragment.
func substitute(argv []string, fills []string) []string {
	out := make([]string, 0, len(argv))
	next := 0
	for _, arg := range argv {
		if !placeholderPattern.MatchString(arg) {
			out = append(out, arg)
			continue
		}
		value := ""
		if next < len(fills) {
			value = fills[next]
		}
		next++
		replaced := placeholderPattern.ReplaceAllString(arg, value)
		// An empty optional field (a denial with no reason given) should
		// drop the argument rather than pass "".
		if strings.TrimSpace(replaced) == "" {
			continue
		}
		out = append(out, replaced)
	}
	return out
}

// placeholders lists the tokens an action needs filled, in argv order.
func placeholders(action feed.Action) []string {
	var out []string
	for _, arg := range action.Run {
		for _, match := range placeholderPattern.FindAllStringSubmatch(arg, -1) {
			out = append(out, match[1])
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

// broadcast delivers text to every marked item that can receive it,
// concurrently. Each send is a model turn that can run for minutes, so doing
// them in sequence would block the UI for as long as the slowest one.
func (m Model) broadcast(text string) tea.Cmd {
	var targets []feed.Action
	for _, item := range m.items {
		if !m.marked[item.Key()] {
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
	argv := substitute(action.Run, []string{text})
	if len(argv) == 0 {
		return fmt.Errorf("action %q has no command", action.Label)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = action.Dir
	return cmd.Run()
}

// markedSendable counts marked items that can actually receive a message.
// Marking a Claude Code session whose pane did not resolve is allowed — it
// just cannot be part of a broadcast, and saying so beats a silent no-op.
func (m Model) markedSendable() int {
	n := 0
	for _, item := range m.items {
		if !m.marked[item.Key()] {
			continue
		}
		if _, ok := sendable(item); ok {
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
	m.fills = nil
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
		m.items, m.results, m.loading = msg.items, msg.results, false
		if m.cursor >= len(m.visible()) {
			m.cursor = max(0, len(m.visible())-1)
		}
		return m, nil

	case tickMsg:
		// Never refetch under an open prompt: the list reordering beneath a
		// half-typed decision is how you approve the wrong thing.
		if m.mode == modeInput || m.mode == modeBroadcast {
			return m, tick()
		}
		return m, tea.Batch(m.load(), tick())

	case ranMsg:
		m.lastErr = msg.err
		m.mode = modeList
		return m, m.load()

	case broadcastMsg:
		m.sent = fmt.Sprintf("sent to %d", msg.ok)
		if msg.failed > 0 {
			m.sent = fmt.Sprintf("sent to %d, %d failed", msg.ok, msg.failed)
			m.lastErr = msg.firstErr
		}
		m.marked = map[string]bool{}
		return m, m.load()

	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mode == modeBroadcast {
		switch msg.Type {
		case tea.KeyEsc:
			m.mode = modeList
			return m, nil
		case tea.KeyEnter:
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				m.mode = modeList
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
			m.fills = append(m.fills, m.input.Value())
			needed := placeholders(m.pending)
			if len(m.fills) < len(needed) {
				m.input.SetValue("")
				m.input.Placeholder = needed[len(m.fills)]
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
		m.loading = true
		return m, m.load()
	case "enter":
		if m.mode == modeDetail {
			m.mode = modeList
		} else if _, ok := m.selected(); ok {
			m.mode = modeDetail
		}
	case "esc":
		m.mode = modeList
	case "1", "2", "3", "4", "5":
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
	t := item.SinceTime()
	if t.IsZero() {
		return ""
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

func clip(s string, n int) string {
	r := []rune(s)
	if n <= 1 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
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
		row := fmt.Sprintf("%-5s %-*s %s", age(item), projWidth,
			clip(project(item), projWidth), clip(item.Title, width-projWidth-24))
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
			b.WriteString(fmt.Sprintf("  [%d] %s %s\n", i+1, a.Label, mutedStyle.Render(strings.Join(a.Run, " "))))
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
