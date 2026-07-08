# Harvest from aispace ("Hive") — features to consider for agent-inbox

> Planning note. agent-inbox is the surviving product; aispace (a Next.js +
> Postgres + Docker web "HQ control-plane") is archived. This doc distills
> every aispace feature worth considering for agent-inbox, with a ruthless
> filter: **agent-inbox is a federator over external agent CLIs, not a web
> platform.** We harvest the supervision/trust/cost/automation ideas and
> deliberately leave the web-platform machinery behind.
>
> Context: Teploy Ship is a coding *agent* (a Claude Code / OpenCode peer),
> not a competitor — it's a future Driver adapter. So "federate Ship + Claude
> + OpenCode" remains agent-inbox's thesis; none of the below should pull it
> toward becoming an execution platform.

## The rule
For each aispace feature, ask: **does this make agent-inbox better at
federating external agent CLIs across a repo portfolio?** If yes → candidate.
If it's multi-tenant/web/DB/exec-sandbox machinery → skip (that's Teploy
Ship + Dash's job, or belongs to the underlying CLIs).

---

## Tier 1 — close these gaps first (high value, on-thesis)

| # | Feature (aispace origin) | What aispace did | agent-inbox version (Go-native) | Effort |
|---|---|---|---|---|
| 1 | **Diff-based review/approval gate** (`TaskDiff`, `approve/reject/force-approve`) | Agent `writeFile` stores old/new as a `TaskDiff`; human approves before it lands; agent cannot self-merge. | Before each `send`, record `git stash create` / HEAD of the project repo. After the driver returns, if the worktree changed, surface it: `review <n>` (full diff), `approve <n>` (commit/keep), `reject <n>` (`git reset --hard` to snapshot). Opt-in per project (`review: true`). Pure git, no DB. **The single highest-value idea here — it's what makes unsupervised sends safe.** | M |
| 2 | **Cost tracking** (`CostRecord`, `calculateCost`, `MODEL_PRICING`) | Per-step token cost logged in a DB tx; hardcoded pricing table for ~30 models across 6 providers. | claude/opencode result JSON already carries `usage`. Parse it, apply a Go pricing map, accumulate per-project in `state.json`, add a `cost` column to `ls` and a `costs` command. Unknown model → conservative fallback like aispace's $5/$15. | S–M |
| 3 | **Budget caps + circuit breaker** (`tokenBudget`, `sessionBudget`, HTTP 402) | Per-task/per-session/monthly caps; refuse execution on exceed. | Per-project config `budget_monthly_usd` / `budget_per_send_usd`. **Before** a send, check monthly spend; refuse to start if over. Honest limitation: a running external CLI can't be hard-stopped mid-flight, so the gate is at send-start, not mid-turn (document this). | S |
| 4 | **Failure classification + retry/backoff** (`failure-types.ts`, `calculateRetryDelay`) | 6 classes (AUTH/BILLING/RATE_LIMIT/TIMEOUT/FORMAT/CRASH) with retryability + exponential backoff + jitter. | Classify `Result.Err` from driver stderr patterns into the same classes; auto-retry retryable classes with exp+backoff+jitter; config `retries: N`. | S–M |
| 5 | **Lifecycle hooks** (`HookRegistry`, 50 events) | Priority-ordered handlers across agent/task/cost/directive lifecycle. | A small event set — `on-send`, `on-waiting`, `on-complete`, `on-error`, `on-review-ready` — that exec user-configured shell commands (mirror the inbound Stop-hook bridge, outbound). Lets users wire Slack/ntfy/desktop-notify without baking integrations in. | S |
| 6 | **Outbound webhooks** (`WebhookDispatcher`, HMAC-SHA256 + retries) | Signed POST with `X-Webhook-Signature`, 3 retries, exp backoff, `timingSafeEqual` verify. | Optional `webhooks[]` in config; on each lifecycle event, sign+POST with retry. Build on #5. | S |
| 7 | **Scheduled / cron sends** (`scheduler.ts`) | Simplified cron parser; on match, create+run an auto-execute task. (aispace's was in-memory, literal fields only.) | Per-project `schedule: "<cron>"` + `prompt:` that auto-sends on schedule. Use `robfig/cron` (real ranges/steps, unlike aispace's). Enables "nightly review" / "morning status" workflows — a natural fit for a portfolio supervisor. | M |
| 8 | **Digest / portfolio summary** (`OperatorDigest`, `HQ overview`) | Morning digest: completions, cost, active tasks, autonomy activity per workspace. | A `digest` command (and optional scheduled job) printing across all projects: last activity, completed today, cost today, waiting/blocked. **This is agent-inbox's distinctive angle** — the portfolio view the underlying CLIs can't give you. | S |

## Tier 2 — optional power features (opt-in, don't bloat the core)

| # | Feature (aispace origin) | agent-inbox version | Effort |
|---|---|---|---|
| 9 | **Per-project memory** (`AgentMemory` + `extractFacts`) | A plain `memory.md` per project (facts/decisions, distilled from completed turns), optionally prepended as context to sends. **Skip the embeddings/vector search** — plain-text is enough for a CLI; aispace's BM25+cosine hybrid is overkill here. | S–M |
| 10 | **LLM quality-judge + refinement loop** (`quality-evaluator.ts`, 7 weighted criteria, threshold 8/10, cap 20 iters) | Opt-in per project: after a send completes, run an LLM judge on the git diff; below threshold → auto-send a refinement prompt (cap N). Needs a one-shot LLM call path — agent-inbox is currently driver-only, so reuse a driver in judge mode or add a thin model-caller. Risk: scope creep; keep strictly opt-in. | M–L |
| 11 | **Unified-inbox priority + types** (`UnifiedInbox` itemType/priority) | Tag each waiting project (APPROVAL/QUESTION/ERROR/COMPLETION) + priority; sort/filter in `ls`. | S |
| 12 | **Audit log** (`AuditLogger`) | Append every send + outcome to `~/.agent-inbox/audit.jsonl`. | S |
| 13 | **Prompt templates** (`PromptTemplate`) | Named reusable prompts in config; `send <n> :name` expands. | S |
| 14 | **Coordinator auto-retry/escalation** (`workspace-coordinator.ts`) | A `watch` mode that auto-retries `error` projects up to N, then flags loud. Keep minimal — aispace's three nested autonomy loops are more than a CLI should own. | M |

## Tier 3 — deliberately SKIP (web-platform concerns; would dilute the CLI)

Grouped so the reasoning is explicit. None of these belong in a single-binary terminal federator.

- **Multi-tenancy / auth / RBAC** — Teams, TeamMember roles, Better Auth, Session re-validation, 28-permission RBAC matrix. Single-operator CLI; no auth layer needed.
- **Web UI / realtime** — Next.js/React dashboard, Socket.io + Redis adapter, in-browser node-pty terminal. agent-inbox is terminal-native; `attach` already hands over to the real CLI. (A future optional Bubble Tea TUI is its own effort — aispace itself never built one.)
- **Database** — Postgres/Prisma, 32 models. agent-inbox state is file-based (`state.json`); adding a DB breaks the single-binary ethos.
- **Docker exec sandbox / container pool** — `--network=none`, read-only rootfs, tmpfs `noexec`, stdin-piped exec, pooled containers. agent-inbox doesn't execute code — the external CLIs do, in the repo. **Re-read `docker.ts:54-66` only if agent-inbox ever gains its own exec path** (that's Teploy Ship's territory, not agent-inbox's).
- **Semantic cache / embeddings / vector memory** — Redis SHA cache, OpenAI embeddings, hybrid BM25+cosine. Overkill; the underlying CLIs manage their own context. Plain-text memory (#9) suffices.
- **Business context** — brand voice/colors, industry templates, competitors[], WorkspaceContext. A repo portfolio doesn't need brand-kit config.
- **Rate limiting / quota matrix** — 9-dimension TeamQuota, sliding-window Redis limiters. Cost budgets (#3) cover the real operator need.
- **Storage adapter / backup automation** — local/S3 `CloudStorage`, `pg_dump` + tar retention. N/A: no DB to back up; git is the backup.
- **Prometheus metrics / SSO-SAML / SOC2 / audit-export-to-SIEM** — enterprise concerns, explicitly out of scope for a personal CLI.

---

## Suggested implementation order

1. **Phase 1 — trust + cost** (the biggest gaps): #1 git-diff approval → #2 cost tracking → #3 budget caps → #4 retry/classification. After this, unsupervised sends are safe and accountable.
2. **Phase 2 — automation + integration**: #5 lifecycle hooks → #6 webhooks → #7 cron sends → #8 digest. This is where the "portfolio supervisor" thesis starts paying off.
3. **Phase 3 — memory + intelligence**: #9 per-project memory → #10 quality-judge (strictly opt-in).
4. **Optional UX**: #11 priority/types, #12 audit, #13 templates, #14 watch-mode.

## Drivers (separate track, not from aispace)
Add **Ship** as a first-class Driver alongside claude/opencode (and Codex when ready). The `Driver` interface (`Name / Send / AttachArgs`) is the seam — no aispace-derived work needed, just a new adapter. This is the move that makes agent-inbox "federate all the coding agents," which is its real moat.

---

## Source map (aispace, for reference when implementing)
These are the files to re-read for each idea before building the Go version. aispace lives at `Sides/Archive/aispace/`.
- Approval/diff/cost/spawn loop: `src/lib/agents/executor.ts`
- Pricing table + cost calc: `src/lib/providers.ts`
- Circuit breaker (per-task + per-session caps): `src/lib/agents/circuit-breaker.ts`
- Failure classes + retry delay + model fallback: `src/lib/agents/failure-types.ts`
- Docker isolation patterns (only if ever needed): `src/lib/agents/docker.ts:54-66`
- Quality evaluator (7 criteria): `src/lib/agents/quality-evaluator.ts`
- Refinement loop: `src/lib/autonomy/workspace-coordinator.ts:473-581`
- Memory (hybrid search — we take only the facts idea): `src/lib/agents/memory.ts`
- Cron scheduler: `src/lib/agents/scheduling/scheduler.ts`
- Hook registry: `src/lib/hooks/`
- Webhook dispatcher (HMAC + retry): `src/lib/webhooks/dispatcher.ts`
- Data model (full feature list as schema): `prisma/schema.prisma`
