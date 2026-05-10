---
covers: Multi-phase plan verification discipline — what to verify, where to look, phasing rules
read-when: Planning or reviewing a multi-phase implementation plan
skip-when: Small single-file changes, bug fixes, UI-only work
last-updated: 2026-05-05
---

# Plan Verification Rules

Apply before finalizing any multi-phase plan. Trust-but-verify between scout → planner → final plan.

## Verification discipline (what to verify)

1. **Verify factual claims against code** — re-grep/re-count every number, path, endpoint. Don't copy from scout summaries.
2. **Trace semantics, not just cite lines** — when plan references existing/upstream code, identify WHEN each field mutates and under WHAT conditions. Line-range citation without control-flow trace = how ports silently invert behavior. Check: every call, or specific branches only?
3. **No fabricated identifiers / API families** — every symbol in plan must cite `file:line`. RED FLAGS: plausible-sounding wrappers (`Keyring`, `Validator`, `Manager`), centralized packages (`internal/security`, `internal/auth`) that may be scattered, OTel-style (`StartSpan/EndSpan`) when codebase is emit-based. When unsure, `go doc <pkg>` lists actual exported surface. Apply especially when plan says "reuse existing X".
4. **Struct scope audit before adding state** — verify lifetime (per-request/session/agent/process) before adding a field to an existing struct. "Plausibly per-X" is a red flag — grep construction + ownership. Shared-instance state leaks across isolation boundaries.
5. **Gate-premise test math** — before asserting "feature X triggers independently of Y", list all early-returns from function entry to X. Math-verify any fixture claiming "X without Y".
6. **Port = config-shape match** — "faithful port" divergences in config field name/type are silent breaking changes for users copying upstream config. Match upstream shape, or explicitly flag each divergence with rationale in the phase file.
7. **Verify external API endpoints via `docs-seeker`** — before writing endpoint into plan. Sibling APIs often use different roots.

## Scope & coverage (where to look)

8. **Grep delete scope deep** — `grep -rn '<symbol>' .` whole repo. Stubs often have refs in catalogs/routing/switch cases. Enumerate ALL sites in todo.
9. **Signature-change callers enumeration** — grep + list all callers explicitly. "Update all callers" insufficient.
10. **Alias/shim coverage** — enumerate ALL exported symbols via `go doc <pkg>`. Add compile-time signature guards.
11. **Scout desktop and web separately** — `ui/desktop/frontend/` ≠ `ui/web/`. Different structure, i18n namespaces, test framework presence.

## Phasing & ordering (when)

12. **Re-scout on scope change** — if phase promotes from deferred → active, re-scout. Don't reuse brainstorm summary.
13. **Cross-phase gates explicit** — "Phase N-1 merged + tests green" in phase Context. Execution order alone ≠ enforcement.
14. **Zero-coverage characterization test = blocker step** — write byte/request-body fixture test BEFORE migration. Not "recommended".
15. **i18n keys ordering** — add key + 3 catalogs as explicit todo step BEFORE handler code. Missing key = runtime crash.

## Conventions & finalization

16. **Context key style convention** — check existing `context.go` pattern before introducing new key types. Mixed = code smell.
17. **Verify pass MANDATORY after rewrite** — spawn fresh Explore/grep to audit planner output. Don't trust self-validation.

**Pattern to avoid:** user asks → planner writes → report "done".
**Safer pattern:** user asks → scout → planner writes → audit-verify → report.

**Red-team practice:** After planner completes, run `code-reviewer`/`brainstormer` in audit mode: "spot-check 15+ claims vs live codebase". Past catches: fabricated `crypto.Keyring`/`tracing.StartSpan` (agent-hooks plan); inverted TS-port semantics + wrong struct scope + misread early-return gate (context-pruning plan). See `plans/*/reports/audit-*.md` for concrete examples.
