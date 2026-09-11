# The supervisor — agent-inbox

The supervisor — "the king" in the code — is a session of its own, in a folder
of its own, provisioned at `~/.agent-inbox/supervisor/` on first run. It is not
one of your projects and cannot be removed. Every project you add joins its
fleet.

That folder gets a starter `AGENTS.md` describing the job, which is yours to
edit and is never rewritten. It is the supervisor's only context: it has no
repository, and it cannot read the fleet's files.

**What makes it a king**, mechanically:

1. **State injection.** Each turn is prefixed with the live status and last
   message of every fleet project, so it never has to ask you what is going on.
2. **Directive dispatch.** `[send to PROJECT: message]` lines in its reply are
   parsed out and dispatched. Replies come back as one-line receipts in its
   thread, and the full text goes back to the supervisor to summarize.
3. **Notes.** `[note: ...]` records a durable cross-project fact; `[note drop:
   ...]` retracts one. Notes are injected into later turns and evicted by
   relevance rather than age — the oldest note is usually the most load-bearing
   one, so plain FIFO discards exactly the wrong end.
4. **Standing rules, which you ratify.** `king.constraints` and
   `king.priorities` in your config are injected into *every* turn regardless
   of what it is about — a rule that only applies when its subject happens to
   be in the room is not a rule.

   The supervisor can *propose* one with `[constraint: ...]`, but a proposal
   binds nothing. Press **`m`** from the fleet, then **`a`** to accept, which
   writes it to your config. **`d`** deletes anything — proposal or fact.

   The split is deliberate, and TRUST.md explains why.
5. **Free questions.** `[git: PROJECT status|diff|log]` is answered by
   agent-inbox itself, from a subprocess. Everything else the supervisor wants
   to know costs a model invocation in that project's session; this costs
   milliseconds and no tokens, and returns the same answer every time. It is
   told so explicitly, because a model that does not know the cheap path exists
   will spend a turn on it.

Every project's branch, divergence and dirty state also ride along in the
injected fleet listing, so "is neutron actually mid-change" needs no question at
all. A trailing `*` on a sidebar row means uncommitted changes — the one thing
the status glyph cannot tell you, since an agent that reports it is done and
leaves a clean tree did nothing.

**Capacity.** How much has been spent against the five-hour rate limit is read
from Claude Code's own transcripts and injected too, so the supervisor can
prioritise instead of starting work that stops halfway. Three things about that
number, because a resource figure that looks authoritative and is not gets
trusted at exactly the wrong moment:

- It is **burn, never remaining**. No published denominator exists, so
  "remaining" would be invented. It is labelled as an estimate everywhere.
- It is **deduplicated** on message and request id. The same message really is
  written more than once — twice in a row in one transcript, and again when a
  session is resumed — so summing naively inflates every figure silently.
- Cache reads are **named separately** rather than folded into one total. On
  real transcripts they outweigh everything else fifty to one, and a single
  number would read as fifty times the work actually done.

With no readable source the line is absent rather than zero: "no data" and "no
usage" are the same number and opposite facts. The signed-in account is shown
when it can be determined and reads as unknown when it cannot — attributing one
account's burn to another is worse than admitting ignorance. Switching accounts
is yours to do; the supervisor can recommend it and never performs it.
