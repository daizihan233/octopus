# Octopus

Self-hosted LLM API aggregation and load balancing for individuals. Go backend + Next.js admin UI embedded as a single binary.

Module: `github.com/bestruirui/octopus`  
Toolchain: Go 1.26 (`go.mod`), Node 18+, **pnpm** (not npm/yarn)

## Architecture (must not invent a new one)

```
handlers → op (cache + DB) → model
         → helper (async side effects)
relay    → balancer / transformers / axonhub llm
```

| Path | Role |
|------|------|
| `cmd/` | Cobra CLI (`start`, `version`); `main.go` only calls `cmd.Execute()` |
| `internal/model/` | GORM entities + request DTOs |
| `internal/op/` | Business ops; **cache-first**, write-through / dirty flush on shutdown |
| `internal/relay/` | LLM proxy: parse → balance → forward → stream |
| `internal/relay/balancer/` | Round-robin / random / failover / weighted + circuit + session |
| `internal/server/handlers/` | Admin REST; **self-register in `init()`** |
| `internal/server/router/` | Declarative `GroupRouter` / `Route` registry |
| `internal/server/resp/` | Envelope `{ code, message, data }` |
| `internal/db/migrate/` | Numbered migrations registered in `init()` (before/after AutoMigrate) |
| `internal/task/` | Background sync / health / stats batch write |
| `static/` | `//go:embed all:out` — frontend must land in `static/out/` |
| `web/` | Next.js 16 static export admin UI |

**Two API surfaces (do not mix):**

- Admin: `/api/v1/<resource>/<action>` — JWT (`middleware.Auth`)
- Relay (clients): `/v1/*` — API key (`middleware.APIKeyAuth`)  
  Paths: `chat/completions`, `responses`, `messages`, `embeddings`, `images/*`

LLM protocol conversion comes from `github.com/looplj/axonhub/llm` (fork/adapted). Prefer extending transformers/relay branches over reimplementing providers.

## Commands

**Dev (split processes — preferred for UI work):**

```bash
# terminal 1
cd web && pnpm install && NEXT_PUBLIC_API_BASE_URL="http://127.0.0.1:8080" pnpm run dev
# terminal 2 (repo root)
go run main.go start
# UI: http://localhost:3000  API: http://127.0.0.1:8080
```

**Single binary (frontend must be built first):**

```bash
cd web && pnpm install && pnpm run build && cd ..
# output must be static/out (embed path)
# Windows-friendly equivalent of README's `mv web/out static/`:
Remove-Item -Recurse -Force static/out -ErrorAction SilentlyContinue
Move-Item web/out static/out
go run main.go start
```

**Frontend only:**

```bash
cd web && pnpm run lint    # eslint
cd web && pnpm run build   # next build → web/out
```

**Release / multi-arch (Linux CI script):**

```bash
bash scripts/build.sh release          # all platforms
bash scripts/build.sh <os> <arch>      # single target
# build.sh also runs python3 scripts/updatePrice.py before go build
# go build flags: CGO_ENABLED=0 -tags=jsoniter + version ldflags into internal/conf
```

**Hot reload:** `air` uses `.air.toml` — rebuilds frontend into `static/out` then `go build` (Windows-oriented paths).

There is **no Makefile**, no golangci-lint config, and essentially **no Go test suite**. Do not invent a test harness unless asked. Verify with `go build` / `pnpm run build` / `pnpm run lint` as appropriate.

## Config & runtime

- Config file: `data/config.json` (auto-created on first start). Env override: `OCTOPUS_` + path with `_` (e.g. `OCTOPUS_SERVER_PORT`).
- Default admin: `admin` / `admin`.
- DB: sqlite (default), mysql, postgres via GORM; MySQL/Postgres DBs must exist first.
- Stats/op caches live in memory and flush on interval + graceful shutdown (`shutdown.Register`). **Never recommend `kill -9`** for local runs when stats matter.
- Runtime data dirs to leave alone: `data/`, `static/out/`, `tmp/`, `build/`.

## Conventions agents miss

### Backend

1. **New admin endpoint:** add/extend a file under `internal/server/handlers/`, register routes in `init()` with `router.NewGroupRouter(...).Use(...).AddRoute(...)`. Handlers package is blank-imported from `server.go` — no central route table.
2. **Responses:** always `resp.Success` / `resp.Error`; prefer shared strings in `resp` for common errors.
3. **Data changes:** `model/` + `op/` (+ migration in `internal/db/migrate/` if schema changes). Follow existing partial-update DTOs (pointer optional fields + explicit add/update/delete key lists).
4. **New channel type:** model const + `relay` branch (+ frontend `ChannelType`) — touch all three layers.
5. **Side effects** (fetch models, delay probes): `go func` + timeout context from handlers/helper, not inside hot relay path without matching existing patterns.
6. **Naming:** op functions often `NounVerb` (`ChannelList`, `ChannelCreate`). JSON/GORM tags snake_case. Domain comments are often Chinese — match surrounding file language.
7. **Logging:** `internal/utils/log` (zap wrappers: `Infof` / `Errorf` / …).

### Frontend (`web/`)

1. Package manager is **pnpm** only; lockfile is `web/pnpm-lock.yaml`.
2. Static export: `output: "export"` in `next.config.ts`. No Next server routes for API — all data via backend.
3. API layer: `web/src/api/client.ts` unwraps `{ data }`; endpoints live in `web/src/api/endpoints/*.ts` as React Query hooks with keys like `['channels', 'list']`.
4. Feature UI: `web/src/components/modules/<feature>/`; primitives in `components/ui/` (shadcn/new-york).
5. State: Zustand for auth/settings; TanStack Query for server data. Invalidate related query keys after mutations.
6. Alias: `@/*` → `src/*`. Stack: React 19, Tailwind 4, next-intl, recharts, motion.

## Anti-patterns

- Do not put admin routes under `/v1` or relay under `/api/v1`.
- Do not skip building/moving frontend into `static/out` before claiming a production-style Go binary works.
- Do not bypass `op` cache layer with ad-hoc GORM in handlers (existing code goes handlers → op).
- Do not add npm/yarn lockfiles or a second UI framework.
- Do not force-kill the process in docs/scripts that care about stats flush.
- One PR = one theme (`CONTRIBUTING.md`); AI-assisted changes need human review before submit.

## CI / release

**Two workflows, different triggers:**

| Workflow | File | Trigger | What it does |
|----------|------|---------|-------------|
| `release` | `.github/workflows/release.yaml` | push to `master` / manual dispatch | `scripts/build.sh release` → GitHub Release assets + Docker (Alpine + Debian) to GHCR/Docker Hub |
| `changelog` | `.github/workflows/changelog.yml` | push tag `v*` | `changelogithub` creates a GitHub Release with auto-generated notes → force-merges `dev` → `master` |

**Correct release flow:**
1. Commit + push to `dev`
2. `git checkout master && git merge dev && git push` — triggers `release.yaml`
3. `git tag v0.10.XX && git push origin v0.10.XX` — triggers `changelog.yml` which creates the Release + merges dev back to master

⚠️ **Don't** manually create a release with `gh release create` before pushing the tag — `changelog.yml` will fail with 422 (duplicate). Let the workflow create it.

⚠️ The `release.yaml` workflow always checks out `ref: master` and uses `git describe --tags --abbrev=0` to find the latest tag. It reuses that tag for the release. It does **not** create new tags.

- Image names in docs may differ (`bestrui/octopus` vs local `kuohu233/octopus` in `docker-compose.yml`); treat compose as a local sample, not the sole source of truth for public tags.

## Where to look first

| Task | Start here |
|------|------------|
| Relay / protocol / LB | `internal/relay/`, `internal/relay/balancer/` |
| Admin API | `internal/server/handlers/`, `internal/server/router/` |
| Schema / persistence | `internal/model/`, `internal/op/`, `internal/db/` |
| Pricing | `internal/price/`, `scripts/updatePrice.py` |
| Admin UI feature | `web/src/components/modules/`, `web/src/api/endpoints/` |
| Embed / SPA serving | `static/static.go`, `internal/server/middleware/static.go` |
| Product docs | `README.md` / `README_zh.md` |
| Task plans (historical) | `docs/plans/` |
