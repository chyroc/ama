# Development

English | [中文](development.zh-CN.md)

## Run it locally

```bash
./scripts/init-env.sh                     # generate .env if missing; dev.sh reads passwords from it
docker compose up -d postgres redis s3    # dependencies only (s3 also creates the ama-net network)
cp server/config.example.yaml server/config.yaml
cd web && npm install && cd ..
./dev.sh                                  # build the frontend → embed it in the backend → start on 0.0.0.0:5273
./dev.sh --skip-web                       # when only Go code changed
./dev.sh --logs / --stop
```

The frontend is embedded in the Go binary with `go:embed`, so there is one process, one port and no HMR:
rerun `./dev.sh` after frontend changes. ⚠️ `dev.sh` kills whatever process is listening on its port
before starting. When running several copies on one machine (for example multiple git worktrees), give
each its own `PORT=` and pass the same `PORT=` to `--stop`.

To use the Docker executor locally, build the session image first: `docker compose build sandbox-image`.

Backend (in `server/`):

```bash
gofmt -l .
go build ./... && go vet ./...
go test -race ./...    # tests that can't reach PG are skipped; point AMA_TEST_PG_HOST / AMA_TEST_PG_PORT at an empty database
```

⚠️ Skipped PG tests show up as green. If you change DDL or SQL in `*_pg.go`, run the tests against a
real PG. The compose PG password is random, so pass
`AMA_TEST_PG_PASSWORD=$(grep ^AMA_PG_PASSWORD= .env | cut -d= -f2-)`.

Frontend (in `web/`):

```bash
npm run typecheck
npm run build
npm run e2e        # Playwright, uses the system Chrome
```

After changing HTTP routes, regenerate the OpenAPI spec and the role permission table (`go test` checks
both):

```bash
cd server && go run ./cmd/gen-openapi && go run ./cmd/gen-authz-capabilities
```

## Where to read next

- [CLAUDE.md](../CLAUDE.md): the rules every change must follow (multi-instance design, schema rules,
  timestamps, commit style). Written for both people and coding agents.
- [docs/guides/](guides/): per-topic guides (backend, frontend, HeroUI, styling, i18n, Docker executor,
  host executor, environment). Mostly in Chinese.
- Each package under `server/` has its own README.

## Layout

```
server/           Go backend (module github.com/chyroc/ama/server)
  cmd/amad/       server entry point
  cmd/ama-cli/    the ama command: used by agents in containers, and the daemon on your own machine
  auth/ authz/    login (local accounts / OIDC / GitHub / API keys) and per-workspace role checks
  core/ feishu/   business logic: workspaces, members, agents, skills, Ark credentials, run history; Feishu bots
  shared/infra/   integrations: pg, redis, s3, ma (Ark), docker / dockertools, shelltools, lark, oidc ...
  repository/schema/  all DDL, one file per table
web/              frontend: Vite + React 19 + TypeScript + Tailwind 4 + HeroUI 3
cli/              npm wrapper for the ama command
sandbox-image/    session container image
deploy/           default config inside the amad image
docs/             design doc and guides
scripts/          icon generation, seed export, Ark agent setup, etc.
```
