# agent-inbox

[![CI](https://img.shields.io/github/actions/workflow/status/im-tyler/agent-inbox/ci.yml?branch=main)](https://github.com/im-tyler/agent-inbox/actions)
[![Release](https://img.shields.io/github/v/release/im-tyler/agent-inbox)](https://github.com/im-tyler/agent-inbox/releases/latest)
![License: MIT](https://img.shields.io/badge/license-MIT-blue)
![Go](https://img.shields.io/badge/Go-single%20binary-00ADD8)
![Status](https://img.shields.io/badge/status-alpha-orange)

**One terminal for every coding agent you have running.**

You have a Claude Code session in one repo, an OpenCode session in another, a
Codex run in a third. Each holds context you cannot see without switching to it,
and each stops to ask you something you will not notice until you do.

agent-inbox puts all of them in one screen, and puts a supervisor in front of
them that can ask them things on your behalf. The supervisor can be the
built-in one — or any agent harness you already use, driving the fleet through
the same operations as CLI verbs or MCP tools ([Any harness as the king](#any-harness-as-the-king)).

```
╭─ agent-inbox ──────────────────────────────────────────────────╮
│                                                                 │
│  king · claude                              fleet               │
│                                                                 │
│  › you                        2:30PM       ★ supervisor         │
│    is anything blocked?                      teploy  ● waiting  │
│                                               neutron ⠸ working │
│  ▸ neutron                    2:31PM                            │
│    api frozen until the db layer lands                          │
│                                                                 │
│  ● king                       2:31PM                            │
│    neutron is frozen until its db layer lands.                  │
│    teploy is waiting on that. nothing else is blocked.          │
│                                                                 │
│  type to talk to king…                                          │
│  enter send    tab fleet    ? help    ctrl+c quit               │
╰─────────────────────────────────────────────────────────────────╯
```

One message went out. The supervisor asked neutron, read the reply, and
answered. You never left the screen.

> **Any harness can be the king.** Every operation is also a CLI verb and an
> MCP tool — `claude mcp add agent-inbox -- agent-inbox mcp` — so Claude Code,
> OpenCode, or any MCP-speaking agent you already trust can supervise the fleet
> instead. The built-in supervisor is optional. See
> [Any harness as the king](#any-harness-as-the-king).

## Why this is not Claude Squad

The existing multi-agent tools decompose **one** project into parallel workers —
a lead agent splits a task and fans it out to short-lived subagents, usually in
one repo, usually over an API.

This is the inverse. Each project is an **independent peer** with its own
long-lived session, its own repo, and its own context that outlives any single
question. Nothing is decomposed. The value is not parallelism, it is not having
to hold six projects in your head at once.

Three things follow from that, and together they are what this tool is:

- **Cross-vendor.** Claude Code, OpenCode and Codex sit behind one driver
  interface, so one supervisor can talk to all three. Vendor-specific code lives
  in `internal/driver/*.go` and nowhere else.
- **Real CLIs, not APIs.** Each project is driven through the actual tool, so it
  inherits that tool's agent loop, permission model and session persistence
  rather than a reimplementation of them.
- **Memory that spans repos.** The supervisor keeps durable cross-project facts
  — the things no single session can know — and curates them itself.

## Install

```sh
go install github.com/im-tyler/agent-inbox@latest
```

Or download a binary for darwin/linux × amd64/arm64 from
[releases](https://github.com/im-tyler/agent-inbox/releases/latest), extract, and
put `agent-inbox` on your `$PATH`.

> `go install` puts binaries in `$(go env GOPATH)/bin`. If the command is not
> found afterwards, that directory is not on your `PATH`.

From source:

```sh
git clone https://github.com/im-tyler/agent-inbox.git
cd agent-inbox && go build -o agent-inbox .
```

`agent-inbox version` works out what it is without being told: a release binary
carries the tag goreleaser stamped, a `go install` binary reads its module
version from build info, and a working-tree build reports the tag it is ahead of
plus `+dirty`.

### Check the install

```sh
agent-inbox doctor
```

This program is mostly a consumer of other people's command-line interfaces, and
those move. `doctor` reports each agent CLI it can find, probes it for the
subcommands and flags this build actually passes, and lists the helper binaries
the session sources shell out to — `sqlite3` for OpenCode's database, `lsof` for
deciding which sessions are live. Without those two the inbox does not error; it
quietly shows less. It also fetches every configured source once, so a parser
that has stopped understanding its input reads as a failure rather than as an
empty list.

## Quickstart

```sh
agent-inbox
```

On first run you get a supervisor and nothing to supervise. Add projects without
touching JSON:

- Press `Tab` to focus the fleet, then `i` for the **session inbox** — every
  agent session on your machine, whatever tool started it. `n` on a row adopts
  it as a project.
- Or `Tab` then `n` to point at a folder directly.

Then type in the box at the bottom. That goes to the supervisor, which can reach
every project you added.

Data lives in `~/.agent-inbox/` — `config.json`, `state.json`, `notes.json` and
`supervisor/`, plus an `events/` directory once the Stop hook starts firing.
Override the location with `AGENT_INBOX_DIR`.

## The supervisor

The supervisor — "the king" in the code — is a session of its own, in a folder
of its own, provisioned at `~/.agent-inbox/supervisor/` on first run. It is not
one of your projects and cannot be removed. Every project you add joins its
fleet.

That folder gets a starter `AGENTS.md` describing the job, which is yours to
edit and is never rewritten. It is the supervisor's only context: it has no
repository, and it cannot read the fleet's files.

**What makes it a king**, mechanically:

1. **State injection.** Each turn is prefixed with the live status and last
   message of every fleet project, so it never has to ask you what is going on.
2. **Directive dispatch.** `[send to PROJECT: message]` lines in its reply are
   parsed out and dispatched. Replies come back as one-line receipts in its
   thread, and the full text goes back to the supervisor to summarize.
3. **Notes.** `[note: ...]` records a durable cross-project fact; `[note drop:
   ...]` retracts one. Notes are injected into later turns and evicted by
   relevance rather than age — the oldest note is usually the most load-bearing
   one, so plain FIFO discards exactly the wrong end.
4. **Standing rules, which you ratify.** `king.constraints` and
   `king.priorities` in your config are injected into *every* turn regardless
   of what it is about — a rule that only applies when its subject happens to
   be in the room is not a rule.

   The supervisor can *propose* one with `[constraint: ...]`, but a proposal
   binds nothing. Press **`m`** from the fleet, then **`a`** to accept, which
   writes it to your config. **`d`** deletes anything — proposal or fact.

   The split is deliberate, and [Trust](#trust) explains why.
5. **Free questions.** `[git: PROJECT status|diff|log]` is answered by
   agent-inbox itself, from a subprocess. Everything else the supervisor wants
   to know costs a model invocation in that project's session; this costs
   milliseconds and no tokens, and returns the same answer every time. It is
   told so explicitly, because a model that does not know the cheap path exists
   will spend a turn on it.

Every project's branch, divergence and dirty state also ride along in the
injected fleet listing, so "is neutron actually mid-change" needs no question at
all. A trailing `*` on a sidebar row means uncommitted changes — the one thing
the status glyph cannot tell you, since an agent that reports it is done and
leaves a clean tree did nothing.

**Capacity.** How much has been spent against the five-hour rate limit is read
from Claude Code's own transcripts and injected too, so the supervisor can
prioritise instead of starting work that stops halfway. Three things about that
number, because a resource figure that looks authoritative and is not gets
trusted at exactly the wrong moment:

- It is **burn, never remaining**. No published denominator exists, so
  "remaining" would be invented. It is labelled as an estimate everywhere.
- It is **deduplicated** on message and request id. The same message really is
  written more than once — twice in a row in one transcript, and again when a
  session is resumed — so summing naively inflates every figure silently.
- Cache reads are **named separately** rather than folded into one total. On
  real transcripts they outweigh everything else fifty to one, and a single
  number would read as fifty times the work actually done.

With no readable source the line is absent rather than zero: "no data" and "no
usage" are the same number and opposite facts. The signed-in account is shown
when it can be determined and reads as unknown when it cannot — attributing one
account's burn to another is worse than admitting ignorance. Switching accounts
is yours to do; the supervisor can recommend it and never performs it.

## Any harness as the king

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
agent-inbox git teploy diff         # the free questions, no agent involved
agent-inbox log tebian --lines 40   # a project's recent conversation
agent-inbox note list               # the durable cross-project facts
agent-inbox add maccel claude ~/code/maccel
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

Eight tools: `fleet_status`, `send`, `git_query`, `history`, `notes_list`,
`note_add`, `note_drop`, `add_project`. The server is stateless per call —
every call is the same one-shot process the CLI verbs are — and it holds no
conversation of its own.

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

## Trust

The supervisor's replies are shaped by what its projects say, and what its
projects say is shaped by the repositories, issues and web pages those agents
read. So everything the supervisor writes is untrusted-derived, and the design
follows from taking that literally.

**Actions.** `[send to ...]` and `[git: ...]` are allowlisted in code, not
trusted to the prompt: the target must be in that turn's fleet, and git's
subcommand comes from a closed set of three. A name appearing in model output is
a name, not authorisation. Nothing assembled from that text reaches git, which
runs as argv and never through a shell.

**Beliefs.** The same stance, applied to memory — which is the part conventional
injection defences miss, because they screen actions rather than what an agent
comes to believe. So:

- **The supervisor cannot author policy.** It proposes; you ratify; ratified
  rules live in `config.json`, the file you own. There is no path from a model
  response to a rule that binds.
- **Observations stay cheap.** Facts are model-written, but they are filtered by
  relevance, age out of a bounded store, and override nothing — they carry their
  own limits, which is exactly what a rule does not.
- **A project's own words are marked wherever they appear**, with a per-line
  `<<<`, in the fleet listing as well as in the fenced reply block. Truncating a
  snippet to 80 characters is no defence: an instruction fits in far fewer.

Deliberately **not** done: screening rule text for anything suspicious.
Published evaluations put detection of this class at roughly half, and weak
signals — a plausible fabricated "the team decided X" with no instruction in it
— are close to indistinguishable from legitimate content. A filter that catches
some of it reads as a guarantee and is not one. Nothing here inspects the text;
the only control is who signed it.

**Round budget.** By default the supervisor gets one dispatch round per message:
ask, read every reply, answer you. `king.rounds` (max 5) lets it act on what a
reply revealed — the case where a project answers "that depends what B is doing"
and it can go ask B instead of telling you to. Each extra round is another N
agent turns spent unattended, which is why the default is 1. A round that
repeats the previous round's dispatch verbatim is stopped as a loop, and a
request that runs out of budget is recorded in the thread rather than dropped in
silence.

**Adoption.** OpenCode and Codex sessions are resumed directly. A Claude Code
row is always a live process, so it is *forked* (`--fork-session`): the new
session inherits the original's history and the original is left untouched.

## Autonomy — the supervisor noticing on its own

Off by default. Switched on, the supervisor starts a turn nobody asked for when
a project in its fleet finishes or gets stuck:

```json
"king": { "rounds": 1, "autonomous": true, "wakes_per_hour": 4 }
```

This is the only thing in agent-inbox that spends money while nobody is
watching, and a supervisor making mediocre decisions unattended is worse than
no supervisor. So the guardrails are the feature, not a later hardening pass:

- **A wake budget per hour**, separate from the round budget — they bound
  different things: how often it may start, and how far it may go once started.
  Exhausting it is written into the thread, never silent.
- **Never while you are typing.** An unsent draft blocks every wake. You are
  mid-thought, and a turn starting under you changes the fleet your message was
  about between writing it and sending it.
- **Coalesced.** Three projects finishing within a few seconds produce one turn
  that sees all of it, not three that each see a slice and dispatch against a
  fleet still in motion.
- **Never while that supervisor is already working**, and only to the group that
  owns the project that changed.
- **Every wake is recorded.** A line goes into the supervisor's own thread
  naming what woke it, before it says anything. A supervisor that acted
  overnight and cannot say why is not auditable, and an unauditable one cannot
  be trusted with more authority than this.

The turn it gets is a different shape from your messages, because there is no
question to answer and nobody watching. It must pick one of four: **unblock**,
**reprompt**, **park**, **escalate** — and it is told to default to park, since
a turn that was not needed costs more than a delay. Only escalate produces text
for you.

## Reference

- [Config](docs/CONFIG.md) — `config.json`, defaults, the data directory
- [Groups](docs/GROUPS.md) — split the fleet between supervisors
- [Session inbox](docs/SESSION_INBOX.md) — `i`, or headless `--json`
- [Keybindings](docs/KEYBINDINGS.md) — every key, both focus modes
- [Hooks](docs/HOOKS.md) — push instead of poll, including hand-run sessions

## Architecture

```
main.go            entry: TUI (default), legacy REPL (--repl), or hook
supervisor.go      provisions each group's supervisor: folder, brief, project
fleet_cmd.go       the headless verbs — status/send/git/log/note/add
mcp_cmd.go         `agent-inbox mcp` — the same operations as MCP tools
inbox_cmd.go       `agent-inbox inbox` — the reader, headless or --json
internal/config    config.json (projects, groups, per-tool settings)
internal/inbox     project state, mutex-guarded; background sends; persistence
                   groups, notes/constraints, the git + usage refresh, autonomy
                   and the multi-client layer: merged saves, send claims,
                   cross-process adoption (multi.go)
internal/driver    Driver interface + adapters (mock, claude, opencode, codex)
internal/git       read-only tree inspection and the fixed [git: ] queries
internal/usage     what has been spent against the rate limit, deduplicated
internal/claim     the cross-process send guard — one writer per session
internal/feed      the teploy.inbox/v1 item shape, merge and sort
internal/sources   session discovery per tool
internal/mux       zellij/tmux pane detection and injection
internal/board     the inbox reader UI — standalone, or hosted by the TUI
internal/tui       Bubble Tea dashboard (model/view/update, styles, run)
```

The reader is one UI with two entry points, not two programs: `i` hosts
`internal/board` as a view, and `agent-inbox inbox` runs the same model
standalone. A second list of the same sessions would only have drifted from
the first.

The same is true of the send path, one layer down: the dashboard, the fleet
verbs and the MCP tools all drive one `Inbox`, and the front-ends are
interchangeable exactly because none of them owns the state. A second
implementation of "send to a project" would have been a second way of racing
the first.

Every adapter implements:

```go
Send(ctx, dir, sessionID, prompt) Result   // empty sessionID = new session
AttachArgs(dir, sessionID) []string        // argv for interactive drop-in
```

Two optional interfaces refine that. `StreamingDriver` reports live activity as
a turn runs, so the UI shows `working · Bash` instead of a silent spinner.
`ForkingDriver` starts a session seeded from another's history, which is how a
live Claude session is adopted without two writers landing on one transcript.

### Verified CLI surfaces

Every claim here was checked by running the tool, not by reading its docs.

- **Claude Code 2.1.220** — `claude -p --output-format json` returns one result
  object (`result`, `session_id`, `is_error`, `permission_denials`).
  `--output-format stream-json` emits NDJSON `system` / `assistant` / `result`
  events. `--resume <id> --fork-session` seeds a new session from a live one and
  returns the new id.
- **OpenCode 1.18.18** — `opencode run --format json` emits NDJSON:
  `step_start` / `tool_use` / `text` / `step_finish`, and **every event carries
  `sessionID`**. That is what the adapter streams from, and it is why the
  session id needs no recovering. `step_finish` also reports `reason` (`stop`
  ends the turn, `tool-calls` ends a step) plus token counts and cost.
  Up to 1.18.11 `--format json` was empty on success, so the blocking path —
  still reached when a turn cannot stream — reads the reply via `opencode
  export <id>` and recovers a new session's id by set-difference of `session
  list` around the run.
- **Codex CLI 0.146.0** — `codex exec --json` emits `thread.started` /
  `item.started` / `item.completed` / `turn.completed`. The conversation id is
  **`thread_id`**, not `session_id`; resume with `codex exec resume <thread_id>`.
  The final message comes from `--output-last-message`, which is the only part
  the CLI guarantees.

## Not yet built

- **Permission policy** — the decision that determines whether this reduces load
  or relocates it. Currently passes through each tool's own mode.
- **OpenCode / Codex stop-equivalents** — a session you run by hand only reports
  in through a Claude Stop hook. OpenCode's `session.idle` event and Codex's
  config-driven hooks are both usable and neither is wired.
- **Multi-host** — projects on other machines over Tailscale.

## License

MIT — see [LICENSE](LICENSE).
