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

	"agentinbox/internal/feed"
	"agentinbox/internal/sources"
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
)

type loadedMsg struct {
	items   []feed.Item
	results []sources.Result
}

type tickMsg time.Time

type ranMsg struct{ err error }

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
}

func New(srcs []sources.Source) Model {
	in := textinput.New()
	in.Prompt = "> "
	in.CharLimit = 500
	return Model{srcs: srcs, input: in, loading: true}
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
	if m.cursor < 0 || m.cursor >= len(m.items) {
		return feed.Item{}, false
	}
	return m.items[m.cursor], true
}

// run executes an action's argv directly — no shell is involved, so a
// substituted reason containing quotes, semicolons or backticks lands as one
// argument and can never become another command.
func run(action feed.Action, fills []string) tea.Cmd {
	argv := substitute(action.Run, fills)
	if len(argv) == 0 {
		return func() tea.Msg { return ranMsg{err: fmt.Errorf("action %q has no command", action.Label)} }
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
		if m.cursor >= len(m.items) {
			m.cursor = max(0, len(m.items)-1)
		}
		return m, nil

	case tickMsg:
		// Never refetch under an open prompt: the list reordering beneath a
		// half-typed decision is how you approve the wrong thing.
		if m.mode == modeInput {
			return m, tick()
		}
		return m, tea.Batch(m.load(), tick())

	case ranMsg:
		m.lastErr = msg.err
		m.mode = modeList
		return m, m.load()

	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
	case "j", "down":
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = max(0, len(m.items)-1)
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
		return "just now"
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

	if len(m.items) == 0 && !m.loading {
		b.WriteString(mutedStyle.Render("  inbox empty\n"))
	}

	width := maxWidth(m.width)
	for i, item := range m.items {
		line := fmt.Sprintf("%s  %-4s  %s", tag(item.Attention), age(item), clip(item.Title, width-34))
		suffix := mutedStyle.Render("  " + item.Origin)
		if i == m.cursor {
			b.WriteString(selectedStyle.Render("> "+line) + suffix + "\n")
		} else {
			b.WriteString("  " + line + suffix + "\n")
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

	b.WriteString("\n" + mutedStyle.Render("j/k move · enter detail · 1-5 act · r refresh · q quit") + "\n")
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
func Run(srcs []sources.Source) error {
	_, err := tea.NewProgram(New(srcs), tea.WithAltScreen()).Run()
	return err
}
