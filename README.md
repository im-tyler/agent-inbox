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
the same operations as CLI verbs or MCP tools ([any harness as the king](docs/HARNESS_KING.md)).

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
> [any harness as the king](docs/HARNESS_KING.md).

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

## How it works

**[The supervisor](docs/SUPERVISOR.md)** is a session of its own, provisioned on first
run. Every turn is prefixed with the live status of the fleet; `[send to PROJECT]`
lines in its replies are dispatched; `[note]`/`[git:]` give it durable facts and
free questions. It can propose rules, but only you ratify one.

**[Any harness as the king](docs/HARNESS_KING.md)** — every operation is also a CLI
verb (`agent-inbox status|send|git|log|note|add`) and an MCP tool
(`agent-inbox mcp`). Claude Code, OpenCode, or any MCP-speaking agent you already
trust can supervise the fleet instead. No daemon: every front-end coordinates
through the same state files.

**[Trust](docs/TRUST.md)** — everything the supervisor writes is untrusted-derived.
Actions are allowlisted in code, git runs as argv never a shell, beliefs and policy
are separated: it can record an observation, it cannot write a rule.

**[Autonomy](docs/AUTONOMY.md)** — off by default. When on, the supervisor wakes
when a project finishes or gets stuck, inside a per-hour budget, never while you
are typing, every wake recorded.

**[Architecture](docs/ARCHITECTURE.md)** — one Go binary: Bubble Tea TUI, a driver
interface per tool (claude/opencode/codex), one send path shared by every
front-end, cross-process claims so two writers never land on one session.

## Reference

- [Config](docs/CONFIG.md) — `config.json`, defaults, the data directory
- [Groups](docs/GROUPS.md) — split the fleet between supervisors
- [Session inbox](docs/SESSION_INBOX.md) — `i`, or headless `--json`
- [Keybindings](docs/KEYBINDINGS.md) — every key, both focus modes
- [Hooks](docs/HOOKS.md) — push instead of poll, including hand-run sessions

## Not yet built

- **Permission policy** — the decision that determines whether this reduces load
  or relocates it. Currently passes through each tool's own mode.
- **OpenCode / Codex stop-equivalents** — a session you run by hand only reports
  in through a Claude Stop hook. OpenCode's `session.idle` event and Codex's
  config-driven hooks are both usable and neither is wired.
- **Multi-host** — projects on other machines over Tailscale.

## License

MIT — see [LICENSE](LICENSE).