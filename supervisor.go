package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/im-tyler/agent-inbox/internal/config"
	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/inbox"
)

// The supervisor is a session in a folder of its own, provisioned here rather
// than configured by the user.
//
// It used to be whichever project came first in config.json. That made a real
// code repo the supervisor by accident, with three consequences that were not
// obvious from the UI: the injected prompt told it that it could not read the
// fleet's files while it sat inside one of their working directories; that
// project was excluded from its own fleet, so it could never be asked about
// itself; and supervision receipts interleaved with that project's own work in
// a single thread.
//
// A folder of its own fixes all three, and gives the supervisor somewhere to
// keep a brief that is authored rather than inherited.

const (
	defaultSupervisorName = "supervisor"
	defaultSupervisorTool = "claude"
)

// supervisorBrief is written once, on first run, and never rewritten — it is
// the user's to edit from that point on. It is deliberately short: the fleet
// state, the directive syntax and the notes are injected into every turn
// already, and repeating them here would give the model two sources that drift.
const supervisorBrief = `# Supervisor

You coordinate a fleet of coding agents. Each one is a separate session in its
own repository; you are not in any of them, and this folder is yours alone.

You cannot read their files. Asking them is the only way to learn anything
about their projects, and the syntax for that is injected into each turn along
with the current state of the fleet.

What you are for:

- Answering questions that span projects, by asking the projects.
- Noticing when one project's work implies something for another.
- Keeping durable cross-project facts, so they are not rediscovered at the cost
  of a round-trip every time.

What you are not for: doing the work yourself. You have no repository to do it
in. Delegate, gather, and report back.
`

// supervisorProject resolves the supervisor's settings, creates its folder and
// brief on first run, and returns it as a project.
//
// A failure to write the brief is not fatal: the folder is what the session
// needs, and a supervisor with no brief still supervises.
func supervisorProject(dataDir string, cfg *config.Settings) *inbox.Project {
	name := cfg.King.Name
	if name == "" {
		name = defaultSupervisorName
	}
	tool := cfg.King.Tool
	if tool == "" {
		tool = defaultSupervisorTool
	}
	dir := cfg.King.Dir
	if dir == "" {
		dir = filepath.Join(dataDir, defaultSupervisorName)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "agent-inbox: warning: supervisor dir %s: %v\n", dir, err)
	} else {
		writeBriefOnce(dir)
	}

	return &inbox.Project{Name: name, Tool: tool, Dir: dir, Status: driver.StatusIdle}
}

// writeBriefOnce writes the starter brief only when neither file is present,
// so an edited brief is never clobbered and a user who prefers the other
// filename does not end up with both.
func writeBriefOnce(dir string) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return
		}
	}
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte(supervisorBrief), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "agent-inbox: warning: supervisor brief: %v\n", err)
	}
}

// withSupervisor puts the supervisor at the head of the project list.
//
// If a configured project already carries the supervisor's name, that one is
// used as-is and nothing is prepended — the user has said explicitly where
// their supervisor lives, and overriding that would be worse than the default
// this replaces.
func withSupervisor(king *inbox.Project, projects []*inbox.Project) []*inbox.Project {
	for _, p := range projects {
		if p.Name == king.Name {
			return projects
		}
	}
	return append([]*inbox.Project{king}, projects...)
}
