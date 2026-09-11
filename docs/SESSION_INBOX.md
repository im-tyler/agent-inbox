# Session inbox — `i`, or headless — agent-inbox

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
