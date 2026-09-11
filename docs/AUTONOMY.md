# Autonomy — the supervisor noticing on its own — agent-inbox

Off by default. Switched on, the supervisor starts a turn nobody asked for when
a project in its fleet finishes or gets stuck:

```json
"king": { "rounds": 1, "autonomous": true, "wakes_per_hour": 4 }
```

This is the only thing in agent-inbox that spends money while nobody is
watching, and a supervisor making mediocre decisions unattended is worse than
no supervisor. So the guardrails are the feature, not a later hardening pass:

- **A wake budget per hour**, separate from the round budget — they bound
  different things: how often it may start, and how far it may go once started.
  Exhausting it is written into the thread, never silent.
- **Never while you are typing.** An unsent draft blocks every wake. You are
  mid-thought, and a turn starting under you changes the fleet your message was
  about between writing it and sending it.
- **Coalesced.** Three projects finishing within a few seconds produce one turn
  that sees all of it, not three that each see a slice and dispatch against a
  fleet still in motion.
- **Never while that supervisor is already working**, and only to the group that
  owns the project that changed.
- **Every wake is recorded.** A line goes into the supervisor's own thread
  naming what woke it, before it says anything. A supervisor that acted
  overnight and cannot say why is not auditable, and an unauditable one cannot
  be trusted with more authority than this.

The turn it gets is a different shape from your messages, because there is no
question to answer and nobody watching. It must pick one of four: **unblock**,
**reprompt**, **park**, **escalate** — and it is told to default to park, since
a turn that was not needed costs more than a delay. Only escalate produces text
for you.
