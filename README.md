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
  "projects": [
    { "name": "tebian",  "tool": "claude",   "dir": "/path/to/tebian" },
    { "name": "neutron", "tool": "opencode", "dir": "/path/to/neutron" },
    { "name": "maccel",  "tool": "codex",    "dir": "/path/to/maccel" }
  ]
}
```

`projects` may be empty — a supervisor with nothing to supervise is a usable
state you add to with `n`. Under `king`, `rounds` is the dispatch budget and the
optional `name`, `tool` and `dir` override the supervisor. OpenCode defaults to a
**free, no-key** model so those projects work without configuring a provider.

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
| `?` | help |
| `Ctrl+C` | quit |

**Fleet focused** (`Tab`) — the sidebar owns the keys:

| Key | Action |
|---|---|
| `j` / `k` | move through the fleet |
| `Enter` | open the selected project's detail view |
| `i` | session inbox |
| `n` | new project |
| `d` | delete · `t` change tool |
| `a` | attach — hands the terminal to the agent, relaunches on exit |
| `x` | cancel an in-flight send, or dismiss a waiting/error badge |
| `Tab` / `Esc` | back to the composer |

**Detail view**: `j`/`k` scroll · `PgDn`/`PgUp` jump 10 · `g`/`G` top/bottom ·
`s` follow-up · `a` attach · `Esc` back.

## Stop hook — push instead of poll

Register `agent-inbox hook` as a Claude `Stop` hook and any Claude session in a
configured project reports "I'm waiting" into the inbox — **including sessions
you run by hand**. It no-ops for any cwd that is not a configured project, so it
is safe to register globally.

```json
{
  "hooks": {
    "Stop": [
      { "hooks": [ { "type": "command", "command": "/abs/path/to/agent-inbox hook" } ] }
    ]
  }
}
```

Session stops → the hook matches cwd to a project (symlink-tolerant), extracts
the last assistant turn, drops an event file in `events/` → the running inbox
ingests it within a second, flips the project to `waiting`, and notifies.

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
- **OpenCode 1.18.11** — `opencode run --format json` is **empty on success**, so
  the adapter ignores run output and reads the reply via `opencode export <id>`.
  A new session's id is recovered by set-difference of `session list` around the
  run, serialized so concurrent projects cannot claim each other's. There is no
  event stream on `run`; `opencode serve` exists for a future adapter.
- **Codex CLI 0.146.0** — `codex exec --json` emits `thread.started` /
  `item.started` / `item.completed` / `turn.completed`. The conversation id is
  **`thread_id`**, not `session_id`; resume with `codex exec resume <thread_id>`.
  The final message comes from `--output-last-message`, which is the only part
  the CLI guarantees.

## Not yet built

- **Permission policy** — the decision that determines whether this reduces load
  or relocates it. Currently passes through each tool's own mode.
- **OpenCode streaming** — no event stream exists on `run`; real streaming means
  driving `opencode serve` over SSE, which is a different transport.
- **OpenCode / Codex stop-equivalents** — OpenCode's CLI has no Stop hook.
  Codex has a config-driven hooks system, not yet wired.
- **Autonomous supervision** — the supervisor acts only when you message it. An
  event-driven king that reacts when a project finishes on its own is the next
  real feature.
- **Multi-host** — projects on other machines over Tailscale.

## License

MIT — see [LICENSE](LICENSE).
