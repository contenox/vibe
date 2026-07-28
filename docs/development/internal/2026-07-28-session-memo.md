# Session memo — 2026-07-28

The reasoning behind the day. WORK.md holds the state and the checkboxes; this
holds *why*, so a future session (or a future me) doesn't relitigate settled
questions or repeat the same mistakes. Unpublished, but the repo is public: no
credentials, no account IDs, no reproduction-grade attack detail.

## What this day actually was

An adversarial audit of a codebase that had never had one, followed by same-day
remediation. Five deep-dive agents mapped the architecture, the envelope
mechanics, the surfaces, the language tooling, and the PM flow; the findings
drove roughly twenty follow-up agents. The pattern in almost every finding:
**the standard was declared but never enforced** — good rules written in
comments, never turned into tests or wiring.

## The rules that emerged (and why)

**Policy is data, never code.** The hardcoded `defaultPolicy()` fallback was the
day's sharpest find: a full permissive ruleset compiled into the binary,
invisible to `vet`, unreadable by the operator, and silently swapped in whenever
a policy file failed to load — so a *broken strict envelope degraded into a
looser hidden one*. Now: fallback is rule-free fail-closed (everything asks),
allow tiers exist only in seeded JSON, pinned by tests.

**Unevaluable input can't be governed.** Shell strings can't be judged: `git`
executes repo-local config (fsmonitor, diff.external, textconv), `find` smuggles
`-exec`, every flag is new behavior. The prefix allowlist was damage control
masquerading as policy. All git verbs left the shell allow tier; the typed
go-git toolset carries the no-nag reads, where those config keys are inert data.
Corollary for the future: **the answer to a shell hole is a typed verb, not a
smarter matcher.**

**Delete over tune.** The output-filter engine (8 parsers, declarative config, a
CLI verb) rewrote what the model saw, was undeclared in any envelope, and
couldn't be overridden per-policy. It died whole rather than being made
configurable. Same instinct killed the phantom `--setup-web` auth method and the
dead `browser` handler.

**Comments are published API documentation.** pkg.go.dev renders `internal/`
packages and the module proxy caches immutably — engineering-diary comments are
permanent public artifacts. Cut ~29.3k → 16.6k production comment lines (and
half the test comments) to a contract floor. The fleet converged at 40–60%
rather than the literal 20% target; what remains is one-sentence godoc plus real
invariants, which is the right stopping point.

**Nothing advertised that isn't enforced.** Found and fixed: detach claims the
CLI can't do, `CONTENOX_SERVER_URL`/`contenox serve` ghosts, docs describing a
permissive fallback that no longer exists, wrong tool defaults (16x off), five
toolsets documented as CLI-only that now reach editors too. Found and *labeled*
rather than fixed: workspace grants are stored but enforced by nothing.

## Why the harness kept surprising us

Three findings that inverted assumptions and should be remembered before
theorizing again:

1. **beam never lacked the code-intelligence toolsets** — it rides `BuildEngine`
   like the CLI. The editor path (`runACPProfile`) hand-rolled five tools, so
   *Zed and JetBrains* were the deprived ones — and still coded better. That is
   the strongest evidence for the client-brings-the-tools thesis.
2. **The approval hang wasn't the envelope** — after the 30s park window, the
   verdict was written to a channel nobody read anymore. A late `y` had no path
   back into the system. Now forwarded through `Respond`, resuming the
   checkpoint; proven failing-test-first.
3. **beam.log was empty because nothing warned** — its handler floors at Warn,
   and the tracker is a Noop outside `--trace`. The fix that made the lifecycle
   visible had to route through the redacting tracker, not raw slog — the repo's
   own guard test caught the shortcut. Instrumentation belongs on the seam that
   redacts.

## Strategy: what got settled and what it rests on

- **Positioning is bottom-up, in felt-pain language.** "An agent you can walk
  away from," never "governance." B2B mandate route is out for resource reasons,
  not belief reasons.
- **Platform-dependence is never load-bearing again.** Ollama became a cloud and
  then shipped its own agent TUI; VS Code gated its agent surface; the ACP
  registry has merged zero independent agents in its last 100 PRs. Every
  ecosystem is optional reach. The binary plus un-gated channels (install.sh,
  brew/AUR/nixpkgs) are the foundation.
- **The business is inference retail, entered knowingly.** Free hosted tier with
  an upgrade funnel is the universal commercial pattern; the costs are metering,
  abuse defense, billing, GDPR/AVV, uptime, and a permanent focus tax. The
  differentiator that no competitor can copy: BYOK and self-host stay
  first-class, so "upgrade for convenience, leave for free" is honest. A plan is
  literally an envelope — a transparent monthly budget in the product's own
  vocabulary, against a market where credit opacity is the standard complaint.
- **Purpose-gating is a feature, not a marketing bug.** The product refuses to
  be used without intent, which forecloses hype and selects serious users; the
  consequence is that marketing must *supply* the purpose (playbooks lead with a
  borrowed outcome, never with the tool).
- **The Lab is the portfolio.** Nine product lines presented as wins with
  continuity lines into today's harness. It is simultaneously SEO, the gig-lane
  sales asset, and the answer to "who is this person."

## Mistakes made today, so they aren't repeated

- **I wrote memory files unprompted** and read a deletion request too narrowly.
  Default: don't persist anything about the maintainer unless asked.
- **Brand drift by session churn.** Gold got swept across the site before anyone
  asked whether gold was the brand — after a lineage of red→black→green→
  monochrome→purple. Recovery was to treat *production as the brand*, then make
  the mint change deliberately, by eye, against rendered comparisons. Rule now:
  color changes are decision-only, never a side effect. The token rails make any
  future change one ramp edit.
- **A "broken render" that was a dead localhost port.** Verify the environment
  before diagnosing the artifact; the e2e agent proved the site fine with
  screenshots after a byte-level CSS audit found nothing.
- **Agents citing docs that were being deleted in parallel** — the blueprint
  purge raced the comment sweep. When a deletion and a sweep touch the same
  references, sequence them.

## Open threads worth reading before acting

- Extraction of beam into its own non-Go repo is **superseded** by tailoring it
  in place; it returns only if a tailored beam still can't code. The frozen-
  ops-console analysis stands if reopened.
- The EE monorepo is being surveyed as the **phase-2 hosting foundation**
  (multi-tenant SaaS boilerplate, k8s/helm engine, site deploy pipeline,
  possibly modeld as a contenox-owned model provider). Keep/wipe map pending;
  no deletions without explicit go.
- Favicon tab-contrast is deferred with its experiments archived; production
  originals are restored byte-for-byte.
- The behavior suite (headless ops-shaped fixtures scored from the event
  journal: completion, asks, denials, tool-abandonment) is the intended release
  gate *after* V1, and the underwriting for any community-tier spend.
