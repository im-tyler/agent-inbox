package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentinbox/internal/config"
	"agentinbox/internal/driver"
	"agentinbox/internal/inbox"
	"agentinbox/internal/tui"
)

func dataDir() string {
	if d := os.Getenv("AGENT_INBOX_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agent-inbox")
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hook":
			runHook()
			return
		case "version", "-version", "--version":
			printVersion()
			return
		}
	}

	dd := dataDir()
	cfgPath := flag.String("config", filepath.Join(dd, "config.json"), "path to config.json")
	statePath := flag.String("state", filepath.Join(dd, "state.json"), "path to state.json")
	replMode := flag.Bool("repl", false, "use the legacy line-oriented REPL instead of the TUI dashboard")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n\nCreate one (see config.example.json) at %s\n", err, *cfgPath)
		os.Exit(1)
	}

	drivers := map[string]driver.Driver{
		"mock":     driver.Mock{},
		"claude":   driver.Claude{PermissionMode: cfg.Claude.PermissionMode},
		"opencode": driver.NewOpenCode(cfg.OpenCode.Model, cfg.OpenCode.SkipPermissions),
	}

	projects := make([]*inbox.Project, len(cfg.Projects))
	for i, p := range cfg.Projects {
		projects[i] = &inbox.Project{Name: p.Name, Tool: p.Tool, Dir: p.Dir, Status: driver.StatusIdle}
	}
	inbox.LoadState(*statePath, projects)

	in := inbox.New(projects, drivers, *statePath)
	eventsDir := filepath.Join(dd, "events")

	if *replMode {
		repl(in, eventsDir)
		return
	}
	if err := tui.Run(in, eventsDir); err != nil {
		fmt.Fprintf(os.Stderr, "agent-inbox: %v\n", err)
		os.Exit(1)
	}
}

// runHook is invoked as a Claude Stop hook. It reads the hook payload from
// stdin, no-ops unless the session's cwd is a federated project, and drops an
// event file for the inbox to ingest.
func runHook() {
	var p struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		CWD            string `json:"cwd"`
	}
	if json.NewDecoder(os.Stdin).Decode(&p) != nil {
		return
	}
	dd := dataDir()
	cfg, err := config.Load(filepath.Join(dd, "config.json"))
	if err != nil {
		return
	}
	tool := ""
	for _, pr := range cfg.Projects {
		if inbox.SameDir(pr.Dir, p.CWD) {
			tool = pr.Tool
			break
		}
	}
	if tool == "" {
		return // not a federated project — stay silent
	}
	_ = inbox.WriteEvent(filepath.Join(dd, "events"), inbox.Event{
		SessionID: p.SessionID,
		Dir:       p.CWD,
		Tool:      tool,
		Message:   inbox.LastAssistantText(p.TranscriptPath),
		TS:        time.Now().Unix(),
	})
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
	argv, dir, err := in.AttachArgs(idx)
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
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
