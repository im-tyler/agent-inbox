# Config — agent-inbox

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
[Groups](GROUPS.md).

Everything under the data directory is written `0600` in directories written
`0700`: it holds assistant output and the paths of your repositories.

If you run with a `--config` somewhere other than the default, set
`AGENT_INBOX_CONFIG` to the same path. The Stop hook is a separate process and
cannot see the flag, so without the variable it reads the default config and
silently matches none of your projects.
