# Trust — agent-inbox

The supervisor's replies are shaped by what its projects say, and what its
projects say is shaped by the repositories, issues and web pages those agents
read. So everything the supervisor writes is untrusted-derived, and the design
follows from taking that literally.

**Actions.** `[send to ...]` and `[git: ...]` are allowlisted in code, not
trusted to the prompt: the target must be in that turn's fleet, and git's
subcommand comes from a closed set of three. A name appearing in model output is
a name, not authorisation. Nothing assembled from that text reaches git, which
runs as argv and never through a shell.

**Beliefs.** The same stance, applied to memory — which is the part conventional
injection defences miss, because they screen actions rather than what an agent
comes to believe. So:

- **The supervisor cannot author policy.** It proposes; you ratify; ratified
  rules live in `config.json`, the file you own. There is no path from a model
  response to a rule that binds.
- **Observations stay cheap.** Facts are model-written, but they are filtered by
  relevance, age out of a bounded store, and override nothing — they carry their
  own limits, which is exactly what a rule does not.
- **A project's own words are marked wherever they appear**, with a per-line
  `<<<`, in the fleet listing as well as in the fenced reply block. Truncating a
  snippet to 80 characters is no defence: an instruction fits in far fewer.

Deliberately **not** done: screening rule text for anything suspicious.
Published evaluations put detection of this class at roughly half, and weak
signals — a plausible fabricated "the team decided X" with no instruction in it
— are close to indistinguishable from legitimate content. A filter that catches
some of it reads as a guarantee and is not one. Nothing here inspects the text;
the only control is who signed it.

**Round budget.** By default the supervisor gets one dispatch round per message:
ask, read every reply, answer you. `king.rounds` (max 5) lets it act on what a
reply revealed — the case where a project answers "that depends what B is doing"
and it can go ask B instead of telling you to. Each extra round is another N
agent turns spent unattended, which is why the default is 1. A round that
repeats the previous round's dispatch verbatim is stopped as a loop, and a
request that runs out of budget is recorded in the thread rather than dropped in
silence.

**Adoption.** OpenCode and Codex sessions are resumed directly. A Claude Code
row is always a live process, so it is *forked* (`--fork-session`): the new
session inherits the original's history and the original is left untouched.
