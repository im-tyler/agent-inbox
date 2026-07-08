# agent-inbox

![CI](https://img.shields.io/github/actions/workflow/status/im-tyler/agent-inbox/ci.yml?branch=main) ![License: MIT](https://img.shields.io/badge/license-MIT-blue) ![Go](https://img.shields.io/badge/Go-single_binary-00ADD8) ![Status](https://img.shields.io/badge/status-alpha-orange)

A federated supervisor for CLI coding agents. One terminal that holds N
independent projects — each backed by its own long-lived **Claude Code** or
**OpenCode** session — and surfaces them through a single inbox: see who's
`waiting` on you, `send` a project a message, `view` its last reply, or
`attach` to drop into the live session.

This is the inverse of the existing tools (Agent Teams, Claude Squad, CAO),
which decompose *one* project into parallel workers. Here each project is an
independent peer with its own context; the value is reducing the cost of
context-switching across a portfolio.

Working name. Phase 1 = Claude + OpenCode.

## Status

| Piece | State |
|---|---|
| Driver interface + abstraction | done |
| Mock driver | done, exercised |
| Claude adapter | **done, live-verified** (send / resume / session persistence) |
| OpenCode adapter | **done, live-verified** (new session + resume + export-based reply, free model) |
| Codex adapter | done, CLI-surface-verified against `codex exec --help`; pending live-run |
| Inbox state model + background sends | done, restart persistence verified |
| StreamingDriver interface (live activity) | done — Claude adapter streams; UI shows `working:Bash` / `working:typing` |
| Per-project message history (last 100 turns) | done |
| TUI dashboard (Bubble Tea) | done — list view, detail view with history, interactive attach |
| Legacy REPL | done, available via `--repl` flag |
| Stop-hook bridge + live notify | **done, live-verified** (Claude only — OpenCode has no Stop hook; Codex hook system exists but not wired) |
| CI + goreleaser + GitHub releases | done |
| Tagged v0.1.0 | done — https://github.com/im-tyler/agent-inbox/releases/tag/v0.1.0 |

### OpenCode notes
- Default model is a **free, no-key** model (`opencode/deepseek-v4-flash-free`),
  so OpenCode projects work without configuring a paid provider. Override per
  install with `opencode.model` in config.
- `opencode run --format json` is **empty on success**, so the adapter ignores
  run output and reads the reply via `opencode export <id>`. A new session's id
  is recovered by set-difference of `session list` around the run (serialized).
- Your paid providers were failing independently of this tool: Z.AI `401`
  (stored api key rejected — `opencode auth login` to refresh) and GitHub
  Copilot `403 not licensed`. Not required for the free default.

## Architecture

```
main.go                 entry: dispatches to TUI (default), legacy REPL (--repl), or hook
internal/config         config.json (projects + per-tool settings)
internal/inbox          project state, mutex-guarded; background Send; persistence
internal/driver         Driver interface + adapters (mock, claude, opencode)
internal/tui            Bubble Tea dashboard (model/view/update, styles, run)
```

The only vendor-specific code lives in `internal/driver/*.go`. Each adapter
implements:

```go
Send(ctx, dir, sessionID, prompt) Result   // empty sessionID = new session
AttachArgs(dir, sessionID) []string          // argv for interactive drop-in
```

A key v1 simplification: in headless one-shot mode, the process **returns when
the turn is done**, so the normalized status is simply `waiting` on success —
sidestepping the fuzzy "blocked vs done vs working" classification until we move
to streaming mode.

### Verified CLI surfaces
- **Claude 2.1.167:** `claude -p --output-format json` returns a single result
  object with `result`, `session_id`, `is_error`, `permission_denials`.
- **OpenCode 1.15.11:** `opencode run --format json` is **empty on success**, so
  the adapter ignores run output and reads the reply via `opencode export <id>`.
  A new session's id is recovered by set-difference of `session list` around
  the run (serialized). `opencode serve` exists for a future persistent-server
  adapter.
- **Codex CLI** (`codex exec --help` surface-verified): `codex exec --json
  --output-last-message <file>` streams JSONL events and writes the final
  assistant message to the given file. Resume: `codex exec resume <session-id>
  <prompt>`. Interactive attach: `codex resume <session-id>`. Sandbox modes:
  `read-only` / `workspace-write` / `danger-full-access` (or
  `--dangerously-bypass-approvals-and-sandbox` for full autonomy).

## Run

### From a release binary

Download the latest archive from [releases](https://github.com/im-tyler/agent-inbox/releases) for your platform (darwin/linux × amd64/arm64), extract, and put `agent-inbox` on your `$PATH`.

### Build from source

```sh
go install github.com/im-tyler/agent-inbox@latest
# or
git clone https://github.com/im-tyler/agent-inbox.git
cd agent-inbox
go build -o agent-inbox .
```

For a release-tagged build (embeds the version string):

```sh
go build -ldflags "-X main.version=v0.1.0" -o agent-inbox .
./agent-inbox version
```

The default UI is a **Bubble Tea TUI dashboard** showing all federated projects
on one screen with live status updates. Pass `--repl` for the legacy
line-oriented REPL. Projects can be added at runtime via `n` — no JSON editing
required (config.json is rewritten on add).

Config:

```json
{
  "claude":   { "permission_mode": "default" },
  "opencode": { "skip_permissions": false },
  "codex":    { "sandbox": "workspace-write" },
  "projects": [
    { "name": "tebian",  "tool": "claude",   "dir": "/path/to/tebian" },
    { "name": "neutron", "tool": "opencode", "dir": "/path/to/neutron" },
    { "name": "maccel",  "tool": "codex",    "dir": "/path/to/maccel" }
  ]
}
```

### TUI keybindings

| Key | Action |
|---|---|
| `j` / `k` or `↑` / `↓` | navigate project list |
| `1` – `9` | select project by index |
| `n` | add a new project (folder + agent picker modal) |
| `s` | send a message to the selected project (inline prompt) |
| `v` or `Enter` | open detail view (full message, metadata, session id, errors) |
| `a` | attach interactively — exits TUI, hands terminal to the agent, re-launches TUI when the agent exits |
| `r` | refresh the toast line |
| `?` | toggle keybindings help |
| `q` or `Ctrl+C` | quit |

While in send mode: `Enter` dispatches, `Esc` cancels.
While in detail view: `Esc` returns to the list.
While in new-project modal: `Enter` advances to the next step (folder → agent → name → add), `Esc` cancels.

### Legacy REPL

```sh
./agent-inbox --repl
```

Commands: `ls`, `send <n> <msg>`, `view <n>`, `attach <n>`, `quit`.

Data dir defaults to `~/.agent-inbox/` (config.json, state.json, events/).
Override with `AGENT_INBOX_DIR`.

## Stop hook — push instead of poll

Register `agent-inbox hook` as a Claude `Stop` hook so any Claude session in a
federated project reports "I'm waiting" into the inbox — **including sessions
you run by hand**, not just inbox-spawned ones. The hook no-ops for any cwd that
isn't a configured project, so it's safe to register globally.

Add to `~/.claude/settings.json` (use the absolute path to the built binary):

```json
{
  "hooks": {
    "Stop": [
      { "hooks": [ { "type": "command", "command": "/abs/path/to/agent-inbox hook" } ] }
    ]
  }
}
```

Flow: session stops -> hook reads the Stop payload, matches cwd to a project
(symlink-tolerant), extracts the last assistant turn from the transcript, drops
an event file in `events/` -> the running inbox's 1s poller ingests it, flips the
project to `waiting`, and prints a live `[notify]`.

## Not yet built (deliberately deferred)
- **Permission policy** — the decision that determines whether this reduces load
  or just relocates it. Currently passes through each tool's own mode.
- **`sharpen`** — optional LLM rewrite of a rough reply before sending.
- **OpenCode Stop-equivalent** — OpenCode's CLI has no Stop hook, so hand-run
  OpenCode sessions don't self-report. No path to fix without upstream CLI
  support.
- **Codex Stop-equivalent** — Codex has a hooks system (config-file-driven,
  not CLI-subcommand-driven) but the bridge into agent-inbox isn't wired yet.
- **OpenCode/Codex streaming** — they don't implement StreamingDriver because
  their CLIs don't expose useful streaming events (OpenCode's `--format json`
  is empty on success; Codex's JSONL is parsed only for session-id recovery).
- **Multi-host** — projects on different machines via Tailscale.
