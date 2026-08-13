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
them that can ask them things on your behalf.

```
╭────────────────────────────────────────────────────────────────────╮
│ agent-inbox                                                        │
│                                                                    │
│ king  claude                              fleet                    │
│                                                                    │
│ › you                             2:30PM  ★ supervisor           · │
│   is anything blocked on neutron?                                  │
│                                           ▸ neutron              ● │
│ ▸ neutron                         2:31PM    api frozen until the   │
│   api frozen until the db layer lands       db layer lands         │
│                                                                    │
│ ▸ teploy                          2:31PM    teploy               ⠹ │
│   waiting on neutron's client                ⠹ Bash                │
│                                                                    │
│ ● claude                          2:32PM    tebian               ● │
│   teploy is blocked on neutron's client      no commits today      │
│   package. tebian is unaffected.                                   │
│                                           3 projects               │
│                                           1 working  2 waiting     │
│                                                                    │
│ type to talk to king...                                            │
│ enter send  alt+enter newline  tab fleet  ? help  ctrl+c quit      │
╰────────────────────────────────────────────────────────────────────╯
```

One message went out. The supervisor asked two projects, read both replies, and
answered. You never left the screen.

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
4. **Free questions.** `[git: PROJECT status|diff|log]` is answered by
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

**Trust.** `[git: ...]` is allowlisted exactly as `[send to ...]` is: the target
must be in that turn's fleet and the subcommand comes from a closed set of
three. The supervisor's reply is model output shaped by agents that have read
repositories, issues and web pages, so a name appearing in it is a name and not
authorisation. Nothing assembled from that text reaches git, which is invoked as
argv and never through a shell.

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

## Groups — more than one supervisor

A supervisor's context is rebuilt from scratch on every turn out of its fleet's
status lines and the notes that mention them. That is what makes supervision
accurate, and it is also what degrades as the fleet grows: seven projects means
seven status lines and every note about any of them, on every message you send.

Groups split the fleet. Each one gets a supervisor of its own and a tab of its
own, so two conversations stay about different things.

```json
"groups": [
  { "name": "infra",   "projects": ["teploy", "infra"] },
  { "name": "product", "projects": ["neutron", "fylun"] }
]
```

```
╭──────────────────────────────────────────────────────────────╮
│ agent-inbox                                                  │
│ infra 1●  ·  product ⠋                                       │
│ infra  claude                              fleet             │
│                                            ★ supervisor-infra│
│                                              teploy        ● │
│                                              infra         · │
│                                            2 projects        │
│                                            1 waiting         │
```

Each group's supervisor is provisioned the same way the single one is — named
`supervisor-<group>`, in a folder of its own. Override any of that with a
`king` block inside the group (`name`, `tool`, `dir`).

A project belongs to exactly one group, and validation rejects a config where
one is claimed twice. A project no group names — including one added later from
the dashboard — joins the first group, so a project is never left in the fleet
with no supervisor able to see it.

`shift+tab` cycles tabs from the composer; `[` and `]` (or `h`/`l`) do it with
the fleet focused. Each tab carries its own count of what is waiting, so a
project needing you in a tab you do not have open still says so.

Omit `groups` entirely for one supervisor over everything, which is the default
and what most installs want.

## Session inbox — `i`, or headless

The dashboard *drives* sessions. The inbox does the opposite: it only reads
state the tools already wrote down, and merges it into one list of what is
waiting on you.

```sh
agent-inbox inbox          # the reader, standalone
agent-inbox inbox --json   # the merged feed, for scripts and agents
```

With no configuration it picks up whichever agent CLIs are installed:

| Source | State signal | Reply from the inbox |
|---|---|---|
| **Claude Code** | `claude agents --json` reports `state: blocked`, with a one-line `needs` from `~/.claude/jobs/` | no — a live session cannot be written into |
| **opencode** | last message's `finish` is `stop` | yes, `opencode run -s <id>` |
| **codex** | rollout ends on a `task_complete` event | yes, `codex exec resume <id>` |
| **teploy-ship** | parked durable runs | yes, approve/deny |

Only sessions whose process is actually running are listed. Claude Code keeps
reporting agents whose process is long gone, still carrying whatever state they
last recorded, and opencode's database holds every session ever created with
almost all of them ended on `stop`. Without a liveness check the list fills with
months of finished work that all looks like it is waiting on you.

Add your own sources in `~/.config/agent-inbox/sources.json` — see
[`sources.example.json`](sources.example.json). Anything speaking the
`teploy.inbox/v1` shape works, over a command or `GET /inbox`; the UI needs no
change, because items carry their own resolve commands. An opencode fork such as
fylun-code works through the `opencode` kind by pointing `opencode_db` at that
build's database.

Actions run as **argv, never through a shell**, so a denial reason full of shell
metacharacters is one argument and can never become another command. A
`{placeholder}` prompts you and is substituted as a single whole argument. An
unreachable source reports itself and never blanks the rest of the list.

## Config

`~/.agent-inbox/config.json`:

```json
{
  "claude":   { "permission_mode": "default" },
  "opencode": { "model": "opencode/deepseek-v4-flash-free", "skip_permissions": false },
  "codex":    { "sandbox": "workspace-write" },
  "king":     { "rounds": 1 },
  "turn_timeout_seconds": 0,
  "projects": [
    { "name": "tebian",  "tool": "claude",   "dir": "/path/to/tebian" },
    { "name": "neutron", "tool": "opencode", "dir": "/path/to/neutron" },
    { "name": "maccel",  "tool": "codex",    "dir": "/path/to/maccel" }
  ]
}
```

There need not be a config file at all: a missing one means the defaults, which
is a supervisor and nothing to supervise. `projects` may likewise be empty — you
add to it with `n`. Under `king`, `rounds` is the dispatch budget and the
optional `name`, `tool` and `dir` override the supervisor; its name is reserved,
so a project may not claim it. `turn_timeout_seconds` bounds one agent turn — 0
means the 30-minute default, -1 means no limit. OpenCode defaults to a **free,
no-key** model so those projects work without configuring a provider. An
optional `groups` array splits the fleet between several supervisors — see
[Groups](#groups--more-than-one-supervisor).

Everything under the data directory is written `0600` in directories written
`0700`: it holds assistant output and the paths of your repositories.

If you run with a `--config` somewhere other than the default, set
`AGENT_INBOX_CONFIG` to the same path. The Stop hook is a separate process and
cannot see the flag, so without the variable it reads the default config and
silently matches none of your projects.

## Keybindings

The main view has two focus modes and `Tab` swaps between them. The footer
always shows the keys for whichever one you are in.

**Composer focused** (the default) — you are talking to the supervisor:

| Key | Action |
|---|---|
| `Enter` | send |
| `Alt+Enter` | newline |
| `PgUp` / `PgDn` | scroll the conversation |
| `Tab` | focus the fleet |
| `Shift+Tab` | next group, when the fleet is split |
| `?` | help |
| `Ctrl+C` | quit |

**Fleet focused** (`Tab`) — the sidebar owns the keys:

| Key | Action |
|---|---|
| `j` / `k` | move through the fleet |
| `[` / `]` or `h` / `l` | previous / next group |
| `Enter` | open the selected project's detail view |
| `i` | session inbox |
| `n` | new project |
| `d` | delete · `t` change tool |
| `a` | attach — hands the terminal to the agent, relaunches on exit |
| `x` | cancel an in-flight send, or dismiss a waiting/error badge |
| `Tab` / `Esc` | back to the composer |

**Detail view**: `j`/`k` scroll · `PgDn`/`PgUp` jump 10 · `g`/`G` top/bottom ·
`s` follow-up · `a` attach · `Esc` back.

## Hooks — push instead of poll

Register `agent-inbox hook` and any Claude session in a configured project
reports into the inbox — **including sessions you run by hand**. It no-ops for
any cwd that is not a configured project, so it is safe to register globally.

```json
{
  "hooks": {
    "Stop": [
      { "hooks": [ { "type": "command", "command": "/abs/path/to/agent-inbox hook" } ] }
    ],
    "Notification": [
      { "hooks": [ { "type": "command", "command": "/abs/path/to/agent-inbox hook --kind notification" } ] }
    ]
  }
}
```

**Stop** means the turn finished. The hook matches cwd to a project
(symlink-tolerant), extracts the last assistant turn, drops an event file in
`events/` → the running inbox ingests it within a second and flips the project
to `waiting`.

**Notification** means it is stuck — on a permission prompt, or on a question.
That is a different event from a reply, even though both leave a project
"waiting", because they call for opposite actions: one has an answer to read,
the other stays stuck until a human says yes. The reason and the specific ask
reach the fleet view and the supervisor's status line, and Claude's
sixty-second idle nudge is filtered out, since "the user has not typed lately"
is not a project needing attention.

This is also the one event allowed to reach a project mid-turn — recording
*why* it is stuck without taking the turn's state from it. A turn blocked on a
prompt otherwise looks exactly like one doing slow work, right up until it hits
the timeout half an hour later.

## Architecture

```
main.go            entry: TUI (default), legacy REPL (--repl), or hook
supervisor.go      provisions the supervisor's folder, brief and project
inbox_cmd.go       `agent-inbox inbox` — the reader, headless or --json
internal/config    config.json (projects + per-tool settings)
internal/inbox     project state, mutex-guarded; background sends; persistence
internal/driver    Driver interface + adapters (mock, claude, opencode, codex)
internal/feed      the teploy.inbox/v1 item shape, merge and sort
internal/sources   session discovery per tool
internal/mux       zellij/tmux pane detection and injection
internal/board     the inbox reader UI — standalone, or hosted by the TUI
internal/tui       Bubble Tea dashboard (model/view/update, styles, run)
```

The reader is one UI with two entry points, not two programs: `i` hosts
`internal/board` as a view, and `agent-inbox inbox` runs the same model
standalone. A second list of the same sessions would only have drifted from the
first.

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
