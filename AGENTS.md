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

**Correct release flow (verified on v0.12.1 — tag-first + manual dispatch):**
1. Commit + push to `dev`
2. `git tag -a v0.12.X -m "v0.12.X"` (pointing at dev HEAD) → `git push origin v0.12.X` — triggers `changelog.yml` which creates the Release + force-merges `dev` → `master`
3. **Manually dispatch the build**: `gh workflow run release.yaml --ref master` — triggers `release.yaml`

⚠️ **Do NOT push master first and tag second.** The `release.yaml` workflow is triggered by pushes to `master` (e.g. the changelog force-merge). But GitHub Actions **does not fire `push` workflows for commits pushed by `GITHUB_TOKEN`** (anti-recursion). So after step 2 you will *not* see a release run appear automatically — check `gh run list`; if absent, dispatch it manually (step 3). By the time the dispatched run checks out and fetches tags, the new tag is already on the remote, so `git describe --tags --abbrev=0` resolves to the new version and everything lands correctly. This is the reliable flow (used for v0.12.1: one clean run).

⚠️ **Don't** manually create a release with `gh release create` before pushing the tag — `changelog.yml` will fail with 422 (duplicate). Let the workflow create it.

⚠️ **Timing race (v0.12.0, seen in practice):** If you instead rely on a real `git push origin master` to trigger `release.yaml` and only push the tag *afterwards*, the run usually checks out + fetches tags **before** the tag has landed — `git describe` falls back to the **previous** tag. Consequences:
   - the archives get uploaded to the *old* release (`softprops/action-gh-release` with `overwrite_files: true` overwrites its assets!), and
   - Docker images get tagged with the old version too, while the new tag's release sits empty.
   Fix: clean up the misplaced assets (`gh release delete-asset <old-tag> <name> --repo <owner>/octopus -y` for each), then re-run `release.yaml` via manual dispatch.

**What `release.yaml` actually does (in order), so you know what to verify:**
   `checkout ref:master` → `git fetch --tags --force` → `build.sh release` (builds frontend + 8 platform archives, `GIT_VERSION` = `git describe --tags --abbrev=0` injected into `internal/conf.Version`) → "Get latest tag" (`git describe --tags --match 'v[0-9]*' --abbrev=0`, stored as `TAG_NAME`) → `Upload Release` (`softprops/action-gh-release`, files=`build/archives/*` → release whose tag is `TAG_NAME`, `overwrite_files: true`) → docker build+push Alpine then Debian (tags `latest`, `latest-alpine`, `$TAG_NAME`, `$TAG_NAME-alpine` → GHCR + Docker Hub).
   The whole job is **one job**; a failure in any later step (often the final Debian docker push hitting GitHub 403 secondary rate-limit) still leaves earlier steps done — e.g. archives already uploaded. If only the docker push failed, the fork is: clean up any misplaced assets, then manually dispatch a fresh `release.yaml` run.

**How to inspect a build:** `gh run view <run-id> --repo <owner>/octopus --json jobs` for step-level outcomes, `--log-failed` for the failing step's log. Common infra failure = GitHub secondary rate-limit (HTTP 403 "You have exceeded a secondary rate limit") on GHCR push — transient, just retry via manual dispatch after a few minutes.

**`changelog.yml`** (push tag `v*`): `changelogithub` creates the Release with auto-generated notes, then force-merges `dev` into `master` (`git merge origin/dev; git push origin master --force`). So after a tagged release, `master` is force-reset to `dev`'s HEAD — keep them in sync before next release.

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
