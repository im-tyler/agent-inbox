# Hooks — push instead of poll — agent-inbox

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
