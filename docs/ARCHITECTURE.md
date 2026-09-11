# Architecture — agent-inbox

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
