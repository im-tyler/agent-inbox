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
