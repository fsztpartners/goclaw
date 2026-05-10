# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## GoClaw Gateway

PostgreSQL multi-tenant AI agent gateway with WebSocket RPC + HTTP API.

## Language

Always respond in the same language as the user's prompt. If the user writes in Vietnamese, respond in Vietnamese. If in English, respond in English.

## Tech Stack

**Backend:** Go 1.26, Cobra CLI, gorilla/websocket, pgx/v5 (database/sql, no ORM), golang-migrate, go-rod/rod, telego (Telegram)
**Web UI:** React 19, Vite 6, TypeScript, Tailwind CSS 4, Radix UI, Zustand, React Router 7. Located in `ui/web/`. **Use `pnpm` (not npm).**
**Desktop:** Wails v2 (`//go:build sqliteonly`), React 19, Framer Motion. Located in `ui/desktop/`.
**Database:** PostgreSQL 18 with pgvector (standard). SQLite via `modernc.org/sqlite` (desktop/lite). Raw SQL with `$1, $2` (PG) or `?` (SQLite) positional params.

## Read-If Table

| Doc file | Read when |
|----------|-----------|
| `docs/00-architecture-overview.md` | Understanding project structure or component relationships |
| `docs/plan-verification-rules.md` | Planning or reviewing a multi-phase implementation |
| `docs/mobile-ui-rules.md` | Modifying web UI components in `ui/web/` |
| `docs/agent-identity-conventions.md` | Working with agent_key vs UUID patterns |
| `docs/06-store-data-model.md` | Adding or modifying database queries/schema |
| `CONTRIBUTING.md` | Creating PRs, releases, CI/CD, branch strategy |

## Key Patterns

- **Store layer:** Interface-based (`store.SessionStore`, etc.) with shared Dialect in `store/base/`. PG (`pg/`) and SQLite (`sqlitestore/`) use `database/sql` + raw SQL, `BuildMapUpdate()` and `BuildScopeClause()` helpers
- **Agent types:** `open` (per-user context, 7 files) vs `predefined` (shared context + USER.md per-user)
- **Agent identity:** UUID for DB/FK/events, agent_key for logs/paths/UI. See `docs/agent-identity-conventions.md`
- **Providers:** Anthropic (native HTTP+SSE), OpenAI-compat, DashScope, Claude CLI, ACP, Codex. All use `RetryDo()`. ProviderAdapter + ModelRegistry forward-compat resolver. Shared SSEScanner in `providers/sse_reader.go`
- **Pipeline:** 8-stage loop (context→history→prompt→think→act→observe→memory→summarize) with pluggable callbacks
- **3-tier memory:** Working → Episodic → Semantic (KG). Progressive loading L0/L1/L2
- **Context propagation:** `store.WithAgentType(ctx)`, `store.WithUserID(ctx)`, `store.WithAgentID(ctx)`, `store.WithLocale(ctx)`, `store.WithTenantID(ctx)`
- **WebSocket protocol:** Frame types `req`/`res`/`event`. First request must be `connect`
- **Config:** JSON5 at `GOCLAW_CONFIG` env. Secrets in `.env.local` or env vars, never in config.json
- **i18n:** Backend: `i18n.T(locale, key, args...)` with catalogs in `internal/i18n/`. Web UI: `i18next` in `ui/web/src/i18n/locales/{en,vi,zh}/`. New strings: add key to `keys.go` + all 3 catalogs before handler code
- **Orchestration:** Delegate tool for inter-agent delegation with agent_links, 3 modes (auto/explicit/manual)
- **Telegram formatting:** LLM output → `SanitizeAssistantContent()` → `markdownToTelegramHTML()` → `chunkHTML()` → `sendHTML()`

## Running

```bash
go build -o goclaw . && ./goclaw onboard && source .env.local && ./goclaw
./goclaw migrate up                 # DB migrations
make test                           # Unit tests
make test-critical                  # P0 + P1 (pre-merge)
cd ui/web && pnpm install && pnpm dev   # Web dashboard (dev)
make desktop-dev                        # Desktop (Wails + SQLite)

# Integration tests (requires pgvector pg18 on port 5433)
docker run -d --name pgtest -p 5433:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=goclaw_test pgvector/pgvector:pg18
TEST_DATABASE_URL="postgres://postgres:test@localhost:5433/goclaw_test?sslmode=disable" \
  go test -v -tags integration ./tests/integration/
```

## Post-Implementation Checklist

```bash
go fix ./...                        # Apply Go version upgrades (run before commit)
go build ./...                      # Compile check (PG build)
go build -tags sqliteonly ./...     # Compile check (Desktop/SQLite build)
go vet ./...                        # Static analysis
```

## Go Conventions

- Use `errors.Is(err, sentinel)` instead of `err == sentinel`
- Use `switch/case` instead of `if/else if` chains on the same variable
- Use `append(dst, src...)` instead of loop-based append
- **Migrations (dual-DB):** PG: add SQL in `migrations/` + bump `RequiredSchemaVersion` in `internal/upgrade/version.go`. SQLite: update `internal/store/sqlitestore/schema.sql` + add patch in `schema.go` `migrations` map + bump `SchemaVersion`. **Always update both** — missing SQLite migrations crash desktop on startup
- **SQL safety:** All user inputs use parameterized queries, never string concatenation. No N+1 queries. WHERE/JOIN/ORDER BY columns must use existing indices
- **DB query reuse:** Check if data is already fetched in current flow before adding a new query. Pass resolved data through context/params
- **Tenant-scope guards:** `RoleAdmin` is not a tenant check. Global tables (no `tenant_id`): gate with `requireMasterScope`. Tenant-scoped tables: `requireTenantAdmin` + SQL `WHERE tenant_id = $N`. See `CONTRIBUTING.md` → "Tenant-Scope Guards"
- **Skip load / stress / benchmark tests** for regular feature work. Use unit + integration tests
