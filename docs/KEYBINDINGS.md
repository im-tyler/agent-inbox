# Keybindings — agent-inbox

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
| `m` | supervisor memory — what it remembers, and `d` to forget one |
| `n` | new project |
| `d` | delete · `t` change tool |
| `a` | attach — hands the terminal to the agent, relaunches on exit |
| `x` | cancel an in-flight send, or dismiss a waiting/error badge |
| `Tab` / `Esc` | back to the composer |

**Detail view**: `j`/`k` scroll · `PgDn`/`PgUp` jump 10 · `g`/`G` top/bottom ·
`s` follow-up · `a` attach · `Esc` back.
