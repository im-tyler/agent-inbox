package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/im-tyler/agent-inbox/internal/config"
	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/ident"
	"github.com/im-tyler/agent-inbox/internal/inbox"
	"github.com/im-tyler/agent-inbox/internal/tui"
	"github.com/im-tyler/agent-inbox/internal/usage"
)

func dataDir() string {
	if d := os.Getenv("AGENT_INBOX_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agent-inbox")
}

// main is a thin wrapper so that every error path leaves through run's
// deferred cleanup. os.Exit does not run deferred functions, so calling it
// from inside the TUI loop skipped Inbox.Close entirely: in-flight sends were
// never cancelled and their child CLI processes were left to the OS.
func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-inbox: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hook":
			runHook(os.Args[2:])
			return nil
		case "inbox":
			runInbox(os.Args[2:])
			return nil
		case "doctor":
			return runDoctor(os.Args[2:])
		case "status", "send", "git", "log", "note", "logbook", "follow", "add":
			return runFleet(os.Args[1:], os.Stdout, os.Stderr)
		case "mcp":
			return runMCP(os.Args[2:], os.Stdout, os.Stderr)
		case "version", "-version", "--version":
			printVersion()
			return nil
		}
	}

	dd := dataDir()
	cfgPath := flag.String("config", defaultConfigPath(dd), "path to config.json")
	statePath := flag.String("state", filepath.Join(dd, "state.json"), "path to state.json")
	replMode := flag.Bool("repl", false, "use the legacy line-oriented REPL instead of the TUI dashboard")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w\n\nEdit %s", err, *cfgPath)
	}
	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("config: %w\n\nEdit %s", err, *cfgPath)
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
	// Supervisors are provisioned, not configured. They are prepended rather
	// than written into config.json's projects: they are not the user's
	// projects, and they should not be removable by editing that list.
	kings, groups, err := supervisors(dd, cfg)
	if err != nil {
		return err
	}
	projects = withSupervisors(kings, projects)
	inbox.LoadState(*statePath, projects)

	in := inbox.New(projects, drivers, *statePath).
		WithConfigPath(*cfgPath).
		WithNotesPath(filepath.Join(dd, "notes.json")).
		WithGroups(groups).
		WithKingRounds(cfg.King.Rounds).
		WithRules(cfg.King.Constraints, cfg.King.Priorities).
		WithTurnTimeout(cfg.TurnTimeout()).
		WithUsage(&usage.Claude{}).
		WithGitRefresh(inbox.DefaultGitRefresh)
	// Autonomy last, and off unless config asks for it. It is the only thing
	// here that starts an agent turn nobody typed a message for.
	in.WithAutonomy(cfg.King.Autonomous, cfg.King.WakesPerHour, in.HasDraft)
	defer in.Close()
	eventsDir := filepath.Join(dd, "events")

	if *replMode {
		repl(in, eventsDir)
		return nil
	}

	// TUI loop: run dashboard; if user requests an attach, exit TUI, exec
	// the interactive child with the terminal handed over, then re-launch.
	for {
		m, err := tui.Run(in, eventsDir)
		if err != nil {
			return err
		}
		req := m.AttachRequest()
		if req == nil {
			return nil
		}
		if err := runAttach(req.Argv, req.Dir); err != nil {
			fmt.Fprintf(os.Stderr, "agent-inbox: attach ended: %v\n", err)
		}
		// The lease held the project's claim for the interactive run's
		// lifetime, refusing managed sends on the same session. It comes off
		// only now that the child is reaped — and a failed release is said,
		// because a lease left held blocks every later managed send.
		if err := req.Lease.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "agent-inbox: attach claim cleanup failed: %v\n", err)
		}
		// An interactive attach advances the real session without telling the
		// dashboard, so the history shown here is no longer the whole
		// conversation. Say so rather than implying completeness.
		in.NoteAttachReturned(req.Project)
	}
}

// defaultConfigPath resolves where config.json lives.
//
// AGENT_INBOX_CONFIG exists because the Stop hook runs as its own process and
// cannot see the --config flag that the TUI was launched with. Without a shared
// resolver a user with a custom config had a hook that silently matched
// nothing.
func defaultConfigPath(dataDir string) string {
	if p := os.Getenv("AGENT_INBOX_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(dataDir, "config.json")
}

// runAttach execs the interactive attach command in the foreground,
// letting the child take over the terminal. Returns when the child exits.
func runAttach(argv []string, dir string) error {
	if len(argv) == 0 {
		return fmt.Errorf("attach: empty argv")
	}
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir = dir
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// runHook is invoked as a Claude hook. It reads the payload from stdin,
// no-ops unless the session's cwd is a federated project, and drops an event
// file for the inbox to ingest.
//
// Two hooks land here, and the difference between them is the point. Stop
// fires when a turn finishes and the session has something to show. Notification
// fires when it is stuck — waiting on permission, or on an answer — and a
// session sitting on a prompt is not the same event as one that replied, even
// though both leave the project "waiting".
//
// The kind is a flag rather than a payload field because Claude does not name
// itself in the payload, and registering the same binary twice is how the two
// hooks are distinguished at the point of registration.
func runHook(args []string) {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	kind := fs.String("kind", "stop", "which hook fired: stop or notification")
	if err := fs.Parse(args); err != nil {
		return
	}

	var p struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		CWD            string `json:"cwd"`
		// Notification carries the prompt text; Stop does not send it.
		Message string `json:"message"`
	}
	if json.NewDecoder(os.Stdin).Decode(&p) != nil {
		return
	}
	dd := dataDir()
	cfg, err := config.Load(defaultConfigPath(dd))
	if err != nil {
		return
	}
	known := false
	for _, pr := range cfg.Projects {
		if ident.SameDir(pr.Dir, p.CWD) {
			known = true
			break
		}
	}
	if !known {
		return // not a federated project — stay silent
	}

	ev := inbox.Event{
		SessionID: p.SessionID,
		Dir:       p.CWD,
		// The tool is "claude" because this is a Claude hook, not because of
		// what the project is configured to use. Labelling a Claude session
		// with a Codex project's tool let a manually-run Claude session in a
		// Codex project's directory overwrite that project's Codex session id.
		Tool: "claude",
		TS:   time.Now().UnixNano(),
	}

	if *kind == "notification" {
		reason, detail := classifyNotification(p.Message)
		// A notification that is neither a permission prompt nor a question is
		// Claude's idle nudge. Filing it would flip a working project to
		// waiting while its turn is still running.
		if reason == "" {
			return
		}
		ev.Reason, ev.Detail = reason, detail
		_ = inbox.WriteEvent(filepath.Join(dd, "events"), ev)
		return
	}

	msg, err := inbox.LastAssistantText(p.TranscriptPath)
	if err != nil {
		// A transcript we cannot read means we do not know what was said. An
		// event with a stale message is worse than no event: it files an old
		// reply against a new turn.
		return
	}
	ev.Reason = inbox.ReasonDone
	ev.Message = msg
	_ = inbox.WriteEvent(filepath.Join(dd, "events"), ev)
}

// classifyNotification decides what a Notification payload means, returning an
// empty reason for the ones that are not worth an event.
//
// Claude sends this hook for two different situations behind one message
// field: a permission request, and a sixty-second idle nudge. Only the first is
// a state change — the second says the user has not typed lately, which is not
// something the fleet should render as a project needing attention.
func classifyNotification(msg string) (inbox.Reason, string) {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "permission"), strings.Contains(low, "approve"):
		return inbox.ReasonPermission, strings.TrimSpace(msg)
	case strings.Contains(low, "waiting for your input"):
		return inbox.ReasonQuestion, strings.TrimSpace(msg)
	default:
		return "", ""
	}
}

func repl(in *inbox.Inbox, eventsDir string) {
	fmt.Println("agent-inbox — federated supervisor. Type 'help' for commands.")
	printList(in)

	done := make(chan struct{})
	go poll(in, eventsDir, done)

	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Printf("\ninbox [%d waiting] > ", in.WaitingCount())
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		cmd, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)

		switch cmd {
		case "help", "h":
			printHelp()
		case "list", "ls", "l":
			printList(in)
		case "send", "s":
			doSend(in, rest)
		case "view", "v":
			doView(in, rest)
		case "attach", "a":
			doAttach(in, rest)
		case "quit", "q", "exit":
			close(done)
			return
		default:
			fmt.Printf("unknown command %q — type 'help'\n", cmd)
		}
	}
	close(done)
}

// poll ingests Stop-hook events and surfaces newly-waiting projects live.
func poll(in *inbox.Inbox, eventsDir string, done <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if upd := in.Ingest(eventsDir); len(upd) > 0 {
				fmt.Printf("\a\n[notify] now waiting: %s\ninbox [%d waiting] > ",
					strings.Join(upd, ", "), in.WaitingCount())
			}
			// The REPL shares the tick's one job with the TUI: notice what
			// other front-ends are doing to the fleet.
			in.RefreshExternal()
		}
	}
}

func printHelp() {
	fmt.Print(`commands:
  ls                 list projects and statuses
  send <n> <msg>     send a message to project n (runs in background)
  view <n>           show project n's last message in full
  attach <n>         drop into project n's live session (hands over terminal)
  quit

Projects also report in via the Stop hook (see README) — sessions you run by
hand in a federated project show up here as 'waiting' automatically.
`)
}

func printList(in *inbox.Inbox) {
	for i, p := range in.Snapshot() {
		fmt.Printf("  %d) %-16s %-9s %-8s %s\n", i+1, p.Name, p.Tool, p.Status, snippet(p))
	}
}

func snippet(p inbox.Project) string {
	s := p.LastMessage
	if p.Status == driver.StatusError && p.LastErr != "" {
		s = "ERR: " + p.LastErr
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 70 {
		s = s[:67] + "..."
	}
	return s
}

func doSend(in *inbox.Inbox, rest string) {
	idxStr, msg, _ := strings.Cut(rest, " ")
	idx, err := strconv.Atoi(idxStr)
	if err != nil || strings.TrimSpace(msg) == "" {
		fmt.Println("usage: send <n> <message>")
		return
	}
	if err := in.Send(idx, strings.TrimSpace(msg)); err != nil {
		fmt.Printf("send: %v\n", err)
		return
	}
	fmt.Printf("sent to project %d (working in background)\n", idx)
}

func doView(in *inbox.Inbox, rest string) {
	idx, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		fmt.Println("usage: view <n>")
		return
	}
	p, err := in.Detail(idx)
	if err != nil {
		fmt.Printf("view: %v\n", err)
		return
	}
	fmt.Printf("--- %s (%s) [%s] session=%s updated=%s ---\n",
		p.Name, p.Tool, p.Status, short(p.SessionID), p.UpdatedAt.Format(time.Kitchen))
	if p.LastErr != "" {
		fmt.Printf("error: %s\n", p.LastErr)
	}
	if p.LastMessage != "" {
		fmt.Println(p.LastMessage)
	}
}

func doAttach(in *inbox.Inbox, rest string) {
	idx, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		fmt.Println("usage: attach <n>")
		return
	}
	argv, dir, lease, err := in.BeginAttach(idx)
	if err != nil {
		fmt.Printf("attach: %v\n", err)
		return
	}
	fmt.Printf("attaching: %s\n", strings.Join(argv, " "))
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir = dir
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		fmt.Printf("attach ended: %v\n", err)
	}
	lease.Release()
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
