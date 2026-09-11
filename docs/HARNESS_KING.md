# Any harness as the king — agent-inbox

The supervisor above is one way to drive the fleet. It is also a CLI session
you have to keep alive, with its own context bill and its own ceiling: status
lines and 80-character snippets, rebuilt every turn. If you are already
sitting in a capable agent — Claude Code, OpenCode, whatever speaks to you —
that agent can be the supervisor instead, and agent-inbox becomes the layer
underneath it:

```sh
agent-inbox status                  # the fleet, live: status, why waiting, git, last message
agent-inbox status --json           # the same, for anything that parses
agent-inbox send neutron "..."      # one real turn; prints the reply, exits
agent-inbox follow --timeout 10m    # sleep until the fleet changes, report the delta
agent-inbox git teploy diff         # the free questions, no agent involved
agent-inbox log tebian --lines 40   # a project's recent conversation
agent-inbox note list               # the durable cross-project facts
agent-inbox logbook --lines 20      # the king's own log — decisions, open threads
agent-inbox add maccel claude ~/code/maccel
```

`status` on a working project includes its **trace** — the tool calls of the
turn in progress, persisted as they happen. "Working" reads as "fourteen
tools in, last one Edit" from any front-end, not just a dashboard that
happens to be open; after the turn, the trace stays with the state so a
harness that wasn't watching can still see what the agent did.

**Waking.** The one thing a harness cannot do on its own is notice things —
nothing pushes to it. `follow` is the answer, and it is deliberately a
blocking command rather than a daemon: it sleeps until a project finishes,
gets blocked on a permission prompt, or starts a turn, then prints the delta
and exits (0 changed, 1 timeout; `--json` for both). A harness that
re-invokes it is event-driven without anyone building it a push channel. It
also ingests pending Stop-hook events itself each tick, so it works with no
other front-end running — a `follow` in the loop is, for event purposes, a
dashboard.

**Continuity.** A harness session ends; the fleet does not. The **logbook**
is the king's own log — decisions, open threads, the why behind this week's
state — appended by whoever is king and read by whoever is next:

```sh
agent-inbox logbook add "parked teploy until neutron's client lands" --as opencode
```

Notes it is not: notes are bounded, curated facts about the fleet, injected
into turns; the logbook is an append-only record of supervising, read on
demand. Together they are everything a new king session needs to pick up the
thread, which is one paste into the harness's own instructions:

```markdown
You are the fleet king. At session start: run `agent-inbox status`,
`agent-inbox logbook --lines 20`, and `agent-inbox note list`, and act on
what changed since the last entry. When you make a decision or leave
something open, record it: `agent-inbox logbook add "..." --as <you>`.
Wake on fleet changes by re-running `agent-inbox follow --timeout 10m`.
```

Every command is one process against the same state the dashboard uses —
there is no daemon, and none is needed, because the three front-ends
(dashboard, these verbs, MCP below) coordinate through the state files:
saves merge per project instead of overwriting, a send *claims* its project
in `~/.agent-inbox/claims/` so two processes can never put two writers on one
session, and the dashboard adopts what other front-ends did within a second.
A harness-driven send that lands while the dashboard is open shows up there
while it runs.

For harnesses that prefer native tools, `agent-inbox mcp` serves the same
operations over the Model Context Protocol:

```sh
claude mcp add agent-inbox -- agent-inbox mcp
```

Ten tools: `fleet_status`, `send`, `git_query`, `history`, `notes_list`,
`note_add`, `note_drop`, `add_project`, `logbook_read`, `logbook_add`. The
server is stateless per call — every call is the same one-shot process the
CLI verbs are — and it holds no conversation of its own. (`follow` is a CLI
verb only: an MCP tool call that blocks for ten minutes is a poor tool.)

Two things a harness-king should know, because they are the terms:

- **The harness's authority is the harness's.** The built-in supervisor is
  deliberately low-authority — an empty folder, three git subcommands,
  proposals that bind nothing. A harness-king runs with everything its
  harness allows, and replies arriving through `send` are marked as reports
  in the tool's description but nothing enforces that reading. If you would
  not let the harness act unattended on one project's say-so, do not let it
  do so on six.
- **Facts stay facts.** `note add` records observations; it cannot write a
  rule. Standing rules are still ratified by you, in config, whoever the
  king is.
