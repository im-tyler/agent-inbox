# agent-inbox

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
| Inbox REPL (ls/send/view/attach) | done, background sends + restart persistence verified |
| State persistence | done |
| Stop-hook bridge + live notify | **done, live-verified** (hand-run session reported into inbox via real transcript) |

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
main.go                 REPL + wiring
internal/config         config.json (projects + per-tool settings)
internal/inbox          project state, mutex-guarded; background Send; persistence
internal/driver         Driver interface + adapters (mock, claude, opencode)
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
- **Claude 2.1.167:** `claude -p --output-format json --session-id <uuid>` /
  `-r <id>` / `--permission-mode`. Result JSON carries
  `result`, `session_id`, `is_error`, `permission_denials`.
- **OpenCode 1.15.11:** `opencode run --format json --session <id>`
  (NDJSON events `{type, timestamp, sessionID, ...}`),
  `--dangerously-skip-permissions`; `opencode serve` exists for a future
  persistent-server adapter.

## Run

```sh
go build -o agent-inbox .
cp config.example.json ~/.agent-inbox/config.json   # then edit projects
./agent-inbox
```

Config:

```json
{
  "claude":   { "permission_mode": "default" },
  "opencode": { "skip_permissions": false },
  "projects": [
    { "name": "tebian",  "tool": "claude",   "dir": "/path/to/tebian" },
    { "name": "neutron", "tool": "opencode", "dir": "/path/to/neutron" }
  ]
}
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
- **OpenCode Stop-equivalent** — the hook bridge is Claude-only so far; OpenCode
  has no Stop hook, so hand-run OpenCode sessions don't yet self-report.
- **Codex adapter** — third driver once the two-vendor abstraction settles.
- **Bubble Tea TUI** — upgrade from the stdlib REPL once the model is right.
- **OpenCode `serve` adapter** — persistent server instead of per-send exec.
