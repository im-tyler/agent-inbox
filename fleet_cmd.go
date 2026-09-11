package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/im-tyler/agent-inbox/internal/config"
	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/git"
	"github.com/im-tyler/agent-inbox/internal/inbox"
	"github.com/im-tyler/agent-inbox/internal/logbook"
	"github.com/im-tyler/agent-inbox/internal/usage"
)

// runFleet is the headless front-end: the same Inbox the dashboard drives,
// driven by one command instead. This is the surface a coding agent's harness
// uses to be the supervisor — status to look, send to ask, git and log for
// what is free, note for what is durable — and it is deliberately built from
// the identical machinery rather than a parallel implementation, because a
// second way of sending would be a second way of racing the first.
//
// Every command is one process: build the inbox, run the operation, close.
// Nothing here needs the TUI running, and everything here is safe with it
// running — sends claim their project, saves merge, mutations take the same
// locks the dashboard does.
func runFleet(argv []string, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		usageFleet(stderr)
		return fmt.Errorf("no command given")
	}
	cmd, rest := argv[0], argv[1:]
	switch cmd {
	case "status":
		return fleetStatus(rest, stdout, stderr)
	case "send":
		return fleetSend(rest, stdout, stderr)
	case "git":
		return fleetGit(rest, stdout, stderr)
	case "log":
		return fleetLog(rest, stdout, stderr)
	case "logbook":
		return fleetLogbook(rest, stdout, stderr)
	case "follow":
		return fleetFollow(rest, stdout, stderr)
	case "note":
		return fleetNote(rest, stdout, stderr)
	case "add":
		return fleetAdd(rest, stdout, stderr)
	case "help", "-h", "--help":
		usageFleet(stdout)
		return nil
	default:
		usageFleet(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usageFleet(w io.Writer) {
	fmt.Fprint(w, `agent-inbox <command> — drive the fleet from anywhere

Usage:
  agent-inbox status [--json]          fleet state: every project, live,
                                       including what a working turn is doing
  agent-inbox send NAME MESSAGE        one turn; prints the reply and exits
                                       MESSAGE of "-" reads stdin
        [--timeout 10m] [--json]
  agent-inbox follow [--timeout 10m]   block until the fleet changes, report
        [--projects a,b] [--json]      what changed; exit 0 changed, 1 not
  agent-inbox git NAME status|diff|log the free questions, no agent involved
  agent-inbox log NAME [--lines 20]    a project's recent conversation
  agent-inbox note [list]              durable cross-project facts
  agent-inbox note add TEXT...         record a fact (facts only — rules are
                                       ratified from config, never written here)
  agent-inbox note drop TEXT           retract every note matching TEXT
  agent-inbox logbook [--lines 20]     the king's own log — decisions, open
                                       threads, read by the next session
  agent-inbox logbook add TEXT [--as AUTHOR]
  agent-inbox add NAME TOOL DIR        federate a project (claude|opencode|codex|mock)

Config is resolved like the hook: $AGENT_INBOX_CONFIG, then the default.
These commands are for agents and scripts; the dashboard remains
`+"`agent-inbox`"+` with no arguments.
`)
}

// fleetInbox builds the one-shot inbox every fleet command runs on. It is
// run()'s construction minus the things a one-process command must not start:
// no autonomy (nothing here should spend money unasked), no background git
// loop (status refreshes synchronously instead).
func fleetInbox() (*inbox.Inbox, error) {
	dd := dataDir()
	cfgPath := defaultConfigPath(dd)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := config.Validate(cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	drivers := map[string]driver.Driver{
		"mock":     driver.Mock{},
		"claude":   driver.Claude{PermissionMode: cfg.Claude.PermissionMode},
		"opencode": driver.NewOpenCode(cfg.OpenCode.Model, cfg.OpenCode.SkipPermissions),
		"codex":    driver.Codex{Model: cfg.Codex.Model, Sandbox: cfg.Codex.Sandbox, SkipApprovals: cfg.Codex.SkipApprovals},
	}
	projects := make([]*inbox.Project, len(cfg.Projects))
	for i, p := range cfg.Projects {
		projects[i] = &inbox.Project{Name: p.Name, Tool: p.Tool, Dir: p.Dir, Status: driver.StatusIdle}
	}
	kings, groups, err := supervisors(dd, cfg)
	if err != nil {
		return nil, err
	}
	projects = withSupervisors(kings, projects)
	statePath := filepath.Join(dd, "state.json")
	inbox.LoadState(statePath, projects)

	in := inbox.New(projects, drivers, statePath).
		WithConfigPath(cfgPath).
		WithNotesPath(filepath.Join(dd, "notes.json")).
		WithGroups(groups).
		WithKingRounds(cfg.King.Rounds).
		WithRules(cfg.King.Constraints, cfg.King.Priorities).
		WithTurnTimeout(cfg.TurnTimeout()).
		WithUsage(&usage.Claude{})
	return in, nil
}

// fleetStatusRow is one project as the machine-readable half of status sees it.
type fleetStatusRow struct {
	Name        string             `json:"name"`
	Tool        string             `json:"tool"`
	Dir         string             `json:"dir"`
	Status      string             `json:"status"`
	WaitReason  string             `json:"wait_reason,omitempty"`
	WaitDetail  string             `json:"wait_detail,omitempty"`
	Git         string             `json:"git,omitempty"`
	LastMessage string             `json:"last_message,omitempty"`
	LastErr     string             `json:"last_err,omitempty"`
	Trace       []inbox.TraceEntry `json:"trace,omitempty"`
	UpdatedAt   time.Time          `json:"updated_at"`
	Supervisor  bool               `json:"supervisor"`
}

// fleetStatusDoc is the whole status answer: the fleet and its partition.
type fleetStatusDoc struct {
	Projects []fleetStatusRow `json:"projects"`
	Groups   []fleetGroupRow  `json:"groups"`
	Usage    string           `json:"usage,omitempty"`
}

type fleetGroupRow struct {
	Name     string   `json:"name,omitempty"`
	King     string   `json:"king"`
	Projects []string `json:"projects"`
}

func fleetStatus(argv []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "machine-readable status on stdout")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	in, err := fleetInbox()
	if err != nil {
		return err
	}
	defer in.Close()
	// Git state is transient and this process just started, so read it now;
	// the dashboard's answer to this is a loop, and a loop is the one thing a
	// one-shot command must not leave behind.
	in.RefreshGit()
	in.RefreshUsage()

	doc := fleetStatusDoc{Groups: []fleetGroupRow{}}
	for _, p := range in.Snapshot() {
		row := fleetStatusRow{
			Name: p.Name, Tool: p.Tool, Dir: p.Dir, Status: string(p.Status),
			WaitReason: string(p.WaitReason), WaitDetail: p.WaitDetail,
			Git: p.Git.Summary(), LastMessage: p.LastMessage, LastErr: p.LastErr,
			Trace: p.Trace, UpdatedAt: p.UpdatedAt, Supervisor: in.IsKing(p.Name),
		}
		doc.Projects = append(doc.Projects, row)
	}
	for i, g := range in.Groups() {
		doc.Groups = append(doc.Groups, fleetGroupRow{
			Name: g.Name, King: g.King, Projects: in.FleetNamesOf(i),
		})
	}
	doc.Usage = in.Usage().Summary()

	if *asJSON {
		return json.NewEncoder(stdout).Encode(doc)
	}
	for _, p := range doc.Projects {
		mark := " "
		if p.Supervisor {
			mark = "★"
		}
		fmt.Fprintf(stdout, "%s %-20s %-9s %-9s %s\n", mark, p.Name, p.Tool, p.Status, p.Git)
		switch {
		case p.WaitDetail != "":
			fmt.Fprintf(stdout, "    blocked: %s\n", oneline(p.WaitDetail, 72))
		case p.Status == "working" && len(p.Trace) > 0:
			last := p.Trace[len(p.Trace)-1]
			fmt.Fprintf(stdout, "    %d tool call(s), last: %s (%s ago)\n",
				len(p.Trace), last.Act, time.Since(last.T).Round(time.Second))
		case p.LastMessage != "":
			fmt.Fprintf(stdout, "    %s\n", oneline(p.LastMessage, 72))
		case p.LastErr != "":
			fmt.Fprintf(stdout, "    error: %s\n", oneline(p.LastErr, 72))
		}
	}
	if doc.Usage != "" {
		fmt.Fprintf(stdout, "capacity (burn, estimated): %s\n", doc.Usage)
	}
	return nil
}

func fleetSend(argv []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 0, "bound the turn, e.g. 45s, 10m (default: the config's turn timeout)")
	asJSON := fs.Bool("json", false, "machine-readable result on stdout")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	args := fs.Args()
	if len(args) < 2 {
		return fmt.Errorf("usage: agent-inbox send NAME MESSAGE  (MESSAGE '-' reads stdin)")
	}
	name, message := args[0], strings.Join(args[1:], " ")
	if message == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("stdin: %w", err)
		}
		message = string(b)
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("empty message")
	}

	in, err := fleetInbox()
	if err != nil {
		return err
	}
	defer in.Close()

	out, ok := in.SendAndWait(name, message, *timeout)
	sendErr := ""
	if out.Err != nil {
		sendErr = out.Err.Error()
	}
	if *asJSON {
		enc, _ := json.Marshal(struct {
			Project   string `json:"project"`
			Status    string `json:"status"`
			SessionID string `json:"session_id,omitempty"`
			Final     string `json:"final,omitempty"`
			Error     string `json:"error,omitempty"`
		}{out.Project, string(out.Status), out.SessionID, out.Final, sendErr})
		fmt.Fprintln(stdout, string(enc))
	} else if sendErr != "" {
		fmt.Fprintf(stderr, "send failed: %s\n", sendErr)
	} else {
		fmt.Fprintln(stdout, out.Final)
	}
	if !ok || sendErr != "" {
		return fmt.Errorf("turn did not complete")
	}
	return nil
}

func fleetGit(argv []string, stdout, stderr io.Writer) error {
	if len(argv) != 2 {
		return fmt.Errorf("usage: agent-inbox git NAME status|diff|log")
	}
	kind, ok := git.ParseKind(argv[1])
	if !ok {
		return fmt.Errorf("%q is not one of status, diff, log", argv[1])
	}
	in, err := fleetInbox()
	if err != nil {
		return err
	}
	defer in.Close()
	answer, err := in.QueryGitByName(argv[0], kind)
	if err != nil {
		return err
	}
	if strings.TrimSpace(answer) == "" {
		answer = "(nothing to report)"
	}
	fmt.Fprintln(stdout, answer)
	return nil
}

func fleetLog(argv []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lines := fs.Int("lines", 20, "how many messages to show")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agent-inbox log NAME [--lines N]")
	}
	in, err := fleetInbox()
	if err != nil {
		return err
	}
	defer in.Close()
	msgs, err := in.HistoryOf(fs.Arg(0), *lines)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		fmt.Fprintf(stdout, "[%s %s]\n%s\n\n", m.Timestamp.Format(time.Kitchen), m.Role, m.Content)
	}
	return nil
}

func fleetNote(argv []string, stdout, stderr io.Writer) error {
	in, err := fleetInbox()
	if err != nil {
		return err
	}
	defer in.Close()

	cmd := "list"
	if len(argv) > 0 {
		cmd, argv = argv[0], argv[1:]
	}
	switch cmd {
	case "list":
		notes := in.Notes()
		if len(notes) == 0 {
			fmt.Fprintln(stdout, "no notes")
			return nil
		}
		for _, n := range notes {
			kind := string(n.Kind)
			if n.Proposed {
				kind += " (proposed — not in effect)"
			}
			fmt.Fprintf(stdout, "%-24s %s\n", kind, n.Text)
		}
		return nil
	case "add":
		if len(argv) == 0 {
			return fmt.Errorf("usage: agent-inbox note add TEXT")
		}
		text := strings.Join(argv, " ")
		before := len(in.Notes())
		in.AddNotes([]string{text})
		if len(in.Notes()) == before {
			return fmt.Errorf("not recorded — it duplicates an existing note")
		}
		fmt.Fprintln(stdout, "recorded")
		return nil
	case "drop":
		if len(argv) == 0 {
			return fmt.Errorf("usage: agent-inbox note drop TEXT")
		}
		n := in.DropNotes([]string{strings.Join(argv, " ")})
		fmt.Fprintf(stdout, "%d note(s) dropped\n", n)
		return nil
	default:
		return fmt.Errorf("unknown note command %q (list, add, drop)", cmd)
	}
}

func fleetAdd(argv []string, stdout, stderr io.Writer) error {
	if len(argv) != 3 {
		return fmt.Errorf("usage: agent-inbox add NAME TOOL DIR  (claude|opencode|codex|mock)")
	}
	in, err := fleetInbox()
	if err != nil {
		return err
	}
	defer in.Close()
	if err := in.AddProject(argv[0], argv[1], argv[2]); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "added %s (%s in %s)\n", argv[0], argv[1], argv[2])
	return nil
}

func oneline(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// fleetFollow is the harness's wake channel: block until the fleet changes,
// report what changed, exit. A harness that re-invokes this on every return
// is effectively event-driven without anybody building it a daemon.
//
// It watches two things, and the second is why this is not just an mtime
// check on state.json: Stop-hook events land in the spool and are applied by
// whichever long-running front-end happens to be alive. When none is — the
// exact situation a headless king is in — the events would sit unapplied and
// state.json would never say a thing. So follow ingests the spool itself
// each tick, with the same tested Ingest the dashboard runs, and then adopts
// whatever other processes wrote. A follow running is, for event purposes, a
// dashboard running.
//
// It exits 0 when something changed and 1 on timeout, so a shell loop and an
// LLM harness can both tell the two apart without parsing.
func fleetFollow(argv []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("follow", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait before reporting no change")
	projects := fs.String("projects", "", "comma-separated names to watch (default: all)")
	asJSON := fs.Bool("json", false, "machine-readable result on stdout")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	in, err := fleetInbox()
	if err != nil {
		return err
	}
	defer in.Close()
	eventsDir := filepath.Join(dataDir(), "events")

	var watch map[string]bool
	if *projects != "" {
		watch = map[string]bool{}
		for _, n := range strings.Split(*projects, ",") {
			if n = strings.TrimSpace(n); n != "" {
				watch[strings.ToLower(n)] = true
			}
		}
	}

	// Baseline after one immediate pass, so an event already sitting in the
	// spool counts as the change it is rather than being silently absorbed.
	in.Ingest(eventsDir)
	in.RefreshExternal()
	base := fleetFingerprint(in.Snapshot())

	deadline := time.Now().Add(*timeout)
	for {
		time.Sleep(time.Second)
		in.Ingest(eventsDir)
		in.RefreshExternal()
		changes := diffFleet(base, fleetFingerprint(in.Snapshot()), in, watch)
		if len(changes) > 0 {
			if *asJSON {
				return json.NewEncoder(stdout).Encode(struct {
					Changed bool             `json:"changed"`
					Changes []fleetChangeRow `json:"changes"`
				}{true, changes})
			}
			for _, c := range changes {
				fmt.Fprintf(stdout, "%s: %s → %s", c.Project, c.From, c.To)
				if c.WaitReason != "" {
					fmt.Fprintf(stdout, " (%s)", c.WaitReason)
					if c.WaitDetail != "" {
						fmt.Fprintf(stdout, " %s", oneline(c.WaitDetail, 100))
					}
				}
				if c.LastMessage != "" {
					fmt.Fprintf(stdout, "\n  %s", oneline(c.LastMessage, 160))
				}
				fmt.Fprintln(stdout)
			}
			return nil
		}
		if time.Now().After(deadline) {
			if *asJSON {
				return json.NewEncoder(stdout).Encode(struct {
					Changed bool             `json:"changed"`
					Changes []fleetChangeRow `json:"changes"`
				}{false, nil})
			}
			return errNoChange
		}
	}
}

// errNoChange says "watched, nothing happened"; it is an exit code, not a
// failure, and is reported as such.
var errNoChange = errors.New("no change within the timeout")

// fleetPin is the slice of a project that follow considers a change: status,
// and why it is waiting. UpdatedAt alone is not one — saves happen for many
// reasons, and a wake that fires on bookkeeping teaches the harness to ignore
// wakes.
type fleetPin struct {
	Status     string
	WaitReason string
	WaitDetail string
}

func fleetFingerprint(snap []inbox.Project) map[string]fleetPin {
	out := make(map[string]fleetPin, len(snap))
	for _, p := range snap {
		out[strings.ToLower(p.Name)] = fleetPin{
			Status:     string(p.Status),
			WaitReason: string(p.WaitReason),
			WaitDetail: p.WaitDetail,
		}
	}
	return out
}

// fleetChangeRow is one observed transition, with enough of the new state to
// act on without a second command.
type fleetChangeRow struct {
	Project     string `json:"project"`
	From        string `json:"from"`
	To          string `json:"to"`
	WaitReason  string `json:"wait_reason,omitempty"`
	WaitDetail  string `json:"wait_detail,omitempty"`
	LastMessage string `json:"last_message,omitempty"`
}

func diffFleet(base, now map[string]fleetPin, in *inbox.Inbox, watch map[string]bool) []fleetChangeRow {
	var out []fleetChangeRow
	for name, pin := range now {
		if watch != nil && !watch[name] {
			continue
		}
		old, seen := base[name]
		if seen && old == pin {
			continue
		}
		row := fleetChangeRow{
			Project: name, To: pin.Status,
			WaitReason: pin.WaitReason, WaitDetail: pin.WaitDetail,
		}
		if seen {
			row.From = old.Status
		}
		for _, p := range in.Snapshot() {
			if strings.EqualFold(p.Name, name) {
				row.LastMessage = oneline(p.LastMessage, 400)
			}
		}
		out = append(out, row)
	}
	return out
}

func fleetLogbook(argv []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("logbook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lines := fs.Int("lines", 20, "how many entries to show")
	as := fs.String("as", "cli", "the author recorded with a new entry")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	path := logbook.Path(dataDir())
	args := fs.Args()
	if len(args) > 0 && args[0] == "add" {
		// flag.Parse stops at the first positional — "add" — so a trailing
		// --as would land in the text rather than naming the author. Pull it
		// out by hand; the documented form is `logbook add TEXT --as AUTHOR`
		// and the documented form has to be the one that works.
		text, author := splitLogbookAddArgs(args[1:], *as)
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("usage: agent-inbox logbook add TEXT [--as AUTHOR]")
		}
		if err := logbook.Append(path, author, text); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "recorded")
		return nil
	}
	entries := logbook.Read(path, *lines)
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "logbook is empty — nothing has been recorded yet")
		return nil
	}
	for _, e := range entries {
		fmt.Fprintf(stdout, "[%s %s] %s\n", e.T.Format("2006-01-02 15:04"), e.Author, e.Text)
	}
	return nil
}

// splitLogbookAddArgs separates the entry text from a trailing --as AUTHOR,
// in both the spaced and = forms.
func splitLogbookAddArgs(args []string, defAuthor string) (text, author string) {
	author = defAuthor
	var words []string
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--as" || args[i] == "-as") && i+1 < len(args):
			author = args[i+1]
			i++
		case args[i] == "--as" || args[i] == "-as":
			// trailing --as with no value: drop rather than record literally
		case strings.HasPrefix(args[i], "--as="):
			author = strings.TrimPrefix(args[i], "--as=")
		default:
			words = append(words, args[i])
		}
	}
	return strings.Join(words, " "), author
}
