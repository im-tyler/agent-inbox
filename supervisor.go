package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/im-tyler/agent-inbox/internal/config"
	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/fsutil"
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

// supervisors provisions one session per group and returns them alongside the
// partition the inbox will run on.
//
// With no groups configured there is exactly one supervisor over everything,
// which is what this program did before groups existed. With groups, each gets
// a supervisor of its own — same provisioning, same folder-per-session rule,
// derived from the group's name.
func supervisors(dataDir string, cfg *config.Settings) ([]*inbox.Project, []inbox.Group, error) {
	if len(cfg.Groups) == 0 {
		name := cfg.King.Name
		if name == "" {
			name = defaultSupervisorName
		}
		dir := cfg.King.Dir
		if dir == "" {
			dir = filepath.Join(dataDir, defaultSupervisorName)
		}
		p, err := supervisorProject(name, cfg.King.Tool, dir)
		if err != nil {
			return nil, nil, err
		}
		return []*inbox.Project{p}, []inbox.Group{{King: name}}, nil
	}

	kings := make([]*inbox.Project, 0, len(cfg.Groups))
	groups := make([]inbox.Group, 0, len(cfg.Groups))
	for _, g := range cfg.Groups {
		name := g.KingName()
		dir := g.King.Dir
		if dir == "" {
			dir = filepath.Join(dataDir, name)
		}
		tool := g.King.Tool
		if tool == "" {
			tool = cfg.King.Tool
		}
		p, err := supervisorProject(name, tool, dir)
		if err != nil {
			return nil, nil, fmt.Errorf("group %q: %w", g.Name, err)
		}
		kings = append(kings, p)
		groups = append(groups, inbox.Group{
			Name:     g.Name,
			King:     name,
			Projects: append([]string(nil), g.Projects...),
		})
	}
	return kings, groups, nil
}

// supervisorProject creates a supervisor's folder and brief on first run, and
// returns it as a project.
//
// A directory that cannot be created is fatal. A supervisor is not an optional
// side feature — its tab's composer sends to it — so continuing with a warning
// produced an application whose primary conversation failed at subprocess
// startup on every message.
//
// A failure to write the brief stays a warning: the folder is what the session
// needs, and a supervisor with no brief still supervises.
func supervisorProject(name, tool, dir string) (*inbox.Project, error) {
	if tool == "" {
		tool = defaultSupervisorTool
	}
	if err := os.MkdirAll(dir, fsutil.DirMode); err != nil {
		return nil, fmt.Errorf("supervisor dir %s: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("supervisor dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("supervisor dir %s exists but is not a directory", dir)
	}
	writeBriefOnce(dir)

	return &inbox.Project{Name: name, Tool: tool, Dir: dir, Status: driver.StatusIdle}, nil
}

// writeBriefOnce writes the starter brief only when neither file is present,
// so an edited brief is never clobbered and a user who prefers the other
// filename does not end up with both.
//
// AGENTS.md is the name written. All three supported CLIs discover it —
// Claude Code hardcodes CLAUDE.md and AGENTS.md discovery — so one file serves
// the supervisor whichever tool it is switched to.
func writeBriefOnce(dir string) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return
		}
	}
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte(supervisorBrief), fsutil.FileMode); err != nil {
		fmt.Fprintf(os.Stderr, "agent-inbox: warning: supervisor brief: %v\n", err)
	}
}

// withSupervisors puts the supervisors at the head of the project list, in
// group order.
//
// Their names are reserved: config.Validate rejects a project that claims one,
// so by the time this runs there is nothing to collide with. It previously
// treated a collision as the user electing that project as their supervisor,
// which is not something the config has any way to say. Naming a repository
// "supervisor" silently suppressed the isolated supervisor, excluded that repo
// from its own fleet, and ran supervision prompts inside a working tree — the
// three problems the dedicated folder exists to prevent.
func withSupervisors(kings, projects []*inbox.Project) []*inbox.Project {
	return append(append([]*inbox.Project(nil), kings...), projects...)
}
