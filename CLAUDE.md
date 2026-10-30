# CLAUDE.md

This file gives guidance to people and coding agents (such as Claude Code) working in this repository.
For an introduction to the project see [README.md](README.md), for deployment and configuration see [docs/deployment.zh-CN.md](docs/deployment.zh-CN.md), and for local development see [docs/development.zh-CN.md](docs/development.zh-CN.md).

## Project intent

AMA platform: Go backend, React (Vite) frontend, built on top of **Ark Managed Agents**.
The agent's tools run in **our own execution environment** (an Ark `self_hosted` environment): by default one
Docker container per session, or a machine the user registers with `ama daemon`.

## Working rules (always on)

1. **Commit after every finished requirement or fix.** One logical change per commit; do not batch unrelated work and do not wait to be asked. Run the checks for what you touched first (see "Common commands"). A commit is local; pushing and opening a pull request still follow "Commits and pull requests".
2. **Never commit company-sensitive or personal information.** That includes internal hostnames and domains, internal service names, tokens / keys / secrets, real people's emails or names, real app / chat / user IDs, and absolute paths under someone's home directory (`/home/<user>/...`, `/Users/<user>/...`, `/data*/home/...`). Use placeholders (`example.com`, `cli_example`, `ou_example`, `/path/to/ama`). Before every commit, read `git diff --cached` and grep it for such patterns.
3. **English for everything that ships in the repository**: commit messages, README / docs you add or change, this CLAUDE.md, and code comments in every language. Log lines, error messages and UI copy are not comments and keep their existing style (UI copy goes through i18n, see `docs/guides/i18n.md`). Existing Chinese docs are translated when you substantially change them.
4. **Refactor regularly.** After each feature lands (and at least every few commits), go over the code you just touched: remove duplication, move shared logic to the package that owns it (for example `server/channel` for anything that is not specific to one chat platform), delete dead code, and keep files focused. Commit refactors on their own as `refactor(scope): ...`, with tests green before and after.
5. **`repos/` holds read-only clones of third-party projects** that we study while building integrations (for example `repos/openclaw-weixin`, the reference for the WeChat channel). It is git-ignored. Never import from it and never commit it; when porting logic, write our own Go code and credit the source (license + link) in the package README.

## This is a distributed service, not a single-machine service

**amad is designed to run as many instances. When writing code, think "the same code is running in N processes at once".**

Running one process locally with `./dev.sh` easily gives you a "single-machine service" instinct, and you then
casually make a batch of decisions that are bound to break once there are several instances. These failures
have one thing in common: **all tests pass on a single instance, the bug shows up only now and then after
deployment, and the error does not point to the real cause**. They look like "sometimes it works, sometimes
it doesn't", the hardest kind to track down.

Three facts mean it has to be written as a distributed service:

1. **Production usually runs several instances**, and the load balancer sends each request to any one of them at random. Two requests in a row are usually not handled by the same process;
2. **During a rolling deploy, old and new instances are online at the same time**, so even if the instance count is set to 1, there is a window where two processes run side by side.
   "I only deploy one instance" is not a reason to keep state in process memory;
3. **Instances can be recreated at any moment** (failed health check, deploy, scale-down). Anything in process memory can vanish without warning, and nobody is told.

### Don't do this / do this

| Case | Don't (single-machine thinking) | Do |
| --- | --- | --- |
| Something that must be remembered across requests | `map` / `sync.Map` / package-level variable | Store it in PostgreSQL (`shared/infra/pg`) |
| Mutual exclusion, leader election, "there can only be one" | `sync.Mutex` | Redis lease (`shared/infra/redis`; the Feishu long connection works this way) |
| Scheduled jobs, background checks | Every instance starts its own ticker | Grab a lock first; if you don't get it, don't run |
| Creating something with a remote side effect | Check first, then create (TOCTOU) | Unique constraint + `ON CONFLICT`, let the database decide |
| Read-modify-write | `SELECT`, change it in Go, then `UPDATE` | A single SQL statement, or a transaction + row lock, or an optimistic version number |
| External callbacks / message dedup | An in-process "seen" set | Store an idempotency key in the database; a unique constraint catches repeated deliveries |
| Long connections / SSE subscribers | An in-process subscriber table is enough | The subscriber is on A and the event is on B; think through how it is stored and how it gets delivered |
| Auto-increment IDs, sequence numbers | `n++` in Go | A database sequence, or something generated without a central point, such as a UUID |
| Temp files, cache directories | Treat them as one shared disk | Each instance has its own disk, lost on recreate; use external storage if it must be shared |

**Only things that can be lost at any time, and whose loss only makes things slower, may live in process memory**
(pure caches, connection pools, the in-between state of the current request).
Anything where "if it's lost, the feature is wrong" must live outside the process.

When you're done, ask yourself: **is this code still correct when two processes run it at the same time?**
If the answer depends on "there is only one instance", it is a bomb waiting to go off.

## Docs to read, by task

**This file holds only what you need to know every time.** Details that only matter for a certain kind of
change live in `docs/guides/` and in each package's README. Before you start, pick from the table below;
almost every item in those docs is a pitfall someone already hit.

| What you are doing | Read first |
| --- | --- |
| Changing Go code in `server/` | [docs/guides/backend.md](docs/guides/backend.md) |
| Changing frontend code in `web/src` | [docs/guides/frontend.md](docs/guides/frontend.md) + `docs/DESIGN.md` (see "Read the conventions before touching UI" below) |
| Using HeroUI components | [docs/guides/heroui.md](docs/guides/heroui.md) |
| Changing theme / colors / dark mode / corner radius | [docs/guides/styling.md](docs/guides/styling.md) |
| Adding or removing UI copy | [docs/guides/i18n.md](docs/guides/i18n.md) |
| Changing the logo / favicon / brand spots | [scripts/brand-icons/README.md](scripts/brand-icons/README.md) |
| Adding a model / changing a model vendor logo | [scripts/model-icons/README.md](scripts/model-icons/README.md) + [server/shared/infra/ma/README.md](server/shared/infra/ma/README.md) |
| Touching the integration layer in `server/shared/infra/` | [docs/guides/infra.md](docs/guides/infra.md) + that package's README |
| Touching the Docker executor / session containers | [docs/guides/docker-executor.md](docs/guides/docker-executor.md) + [server/shared/infra/dockertools/README.md](server/shared/infra/dockertools/README.md) (**docker.sock equals root on the host**) |
| Touching the bring-your-own-machine executor (`ama daemon`) | [docs/guides/host-executor.md](docs/guides/host-executor.md) |
| Adding an HTTP route, changing permissions/roles | [server/authz/README.md](server/authz/README.md) |
| Updating OpenAPI after adding/changing an endpoint, writing notes for agents | [server/openapi/README.md](server/openapi/README.md) |
| Adding/changing a config item, changing where config comes from | [server/shared/infra/config/README.md](server/shared/infra/config/README.md) |
| **Adding a table / changing DDL / finding a table's definition** | [server/repository/schema/README.md](server/repository/schema/README.md) (**spread fields into columns, don't stuff them into a doc jsonb**) |
| **Adding a Store method / adding a storage-layer type / touching json tags of a jsonb doc** | [server/model/persistmodel/README.md](server/model/persistmodel/README.md) |
| Touching PostgreSQL connections / pool / timeouts | [server/shared/infra/pg/README.md](server/shared/infra/pg/README.md) |
| Touching Redis leases / leader election | [server/shared/infra/redis/README.md](server/shared/infra/redis/README.md) |
| Touching object storage / attachment and file bytes / presigned links | [server/shared/infra/s3/README.md](server/shared/infra/s3/README.md) (**bytes in object storage, metadata in PG; the three endpoints must not be mixed up**) |
| Touching any chat channel (shared pieces, adding a platform) | [server/channel/README.md](server/channel/README.md) |
| Touching the Feishu bot | [server/feishu/README.md](server/feishu/README.md) + [server/shared/infra/lark/README.md](server/shared/infra/lark/README.md) |
| Touching the WeChat bot | [server/weixin/README.md](server/weixin/README.md) + [server/shared/infra/weixin/README.md](server/shared/infra/weixin/README.md) |
| Touching credential sources (ama / lark-user, which inject credentials into containers) | [server/core/README-integrations.md](server/core/README-integrations.md) |
| Installing dependencies, building, debugging the environment | [docs/guides/environment.md](docs/guides/environment.md) |
| Changing what problem feedback collects / adding a "dev team only" endpoint | [server/core/README-feedback.md](server/core/README-feedback.md) |
| Touching issues / attachment upload and access | [server/core/README-issue.md](server/core/README-issue.md) |
| Touching product suggestions (users giving feedback on the platform) | [server/core/README-suggestion.md](server/core/README-suggestion.md) (**a third kind of "something is wrong" thing, platform-level; don't call it feedback**) |
| Touching platform self-checks (a scheduled job finds a problem → files an issue automatically) | [server/core/README-inspect.md](server/core/README-inspect.md) |
| Changing the AMA CLI (the `ama` command used by agents inside containers, which is also `ama daemon`) | [server/cmd/ama-cli/README.md](server/cmd/ama-cli/README.md) + [cli/README.md](cli/README.md) (npm wrapper) |
| Setting up agents on Ark with a script | [scripts/setup-ark-agent/README.md](scripts/setup-ark-agent/README.md) |

**If it won't run on your machine, check `docs/guides/environment.md` first.** Most pitfalls there are
"the error does not point to the real cause"; following the error message sends you the long way round.

### Read the conventions before touching UI

**Whenever a change affects how the UI looks** (new page, layout change, new component, colors / corner radius /
spacing, empty and error states, mobile layout), read `docs/DESIGN.md`,
[docs/guides/heroui.md](docs/guides/heroui.md) and [docs/guides/styling.md](docs/guides/styling.md) before you start,
and check against the [HeroUI v3 official docs](https://heroui.com/docs).

The UI in this repository follows a full set of existing conventions (the shape of HeroUI 3 compound components,
`onPress` instead of `onClick`, unlayered CSS beats `@layer` overrides, `Select.Value` renders the whole option,
the last child of `InputGroup` is the Trigger). **If you don't read them you will write something that
"compiles, runs, but doesn't match the rest of the site"**, and that kind of mismatch can't be seen in code
review, only in a screenshot.

## Repository layout

```
server/           Go backend (module: github.com/chyroc/ama/server)
  cmd/amad/       service process entry point
  cmd/ama-cli/    the ama command: used by agents inside containers, also the daemon on bring-your-own machines
  cmd/gen-openapi/ cmd/gen-authz-capabilities/ cmd/gen-doc-outline/  generators (see "Conventions")
  cmd/selfhosted-bootstrap/  small tool to create a self_hosted Ark Environment by hand
  httpd/          HTTP skeleton: routing, CORS, panic recovery, wiring of authentication and authorization
  apihandler/     HTTP handlers
  auth/           authentication: local accounts / OIDC login, sessions, API keys, Protect middleware
  authz/          authorization: workspace member role → permission matrix → Guard wrapper (declare new routes here)
  core/           business logic: workspaces / members / agents / skills / Ark credentials / run history / worker
  service/ orchestrator/  business services split by resource, and logic that spans several services (see each README)
  channel/        what every chat channel shares: run a turn, dedup, session scopes, one loop per account
  feishu/         business logic: Feishu app integration (a channel)
  weixin/         business logic: WeChat bots via the iLink bot API (a channel)
  openapi/        OpenAPI generation and up-to-date check
  model/apimodel/  base types and event definitions shared across modules. **Types that go into HTTP responses live here**
  model/persistmodel/ types used only by the storage layer (Store inputs, row structs that don't go into responses).
                  The test is "does it go into an HTTP response", not "does it have json tags"; see that directory's README
  repository/dao/ PG and in-memory implementations of Store
  repository/schema/ all SQL/DDL, **one table per file, file name is the table name**, each file starts with a generated
                  "table overview". The order they are applied in is an explicit ordered list in schema.go. To add a table see that directory's README
  shared/infra/   **integration layer (does io)**: config (YAML + env vars), lark (Feishu open platform),
                  weixin (WeChat iLink bot protocol, ported from Tencent/openclaw-weixin),
                  ma (Ark Managed Agents), docker (Docker Engine API client),
                  dockertools (default executor: one container per Ark session),
                  shelltools (the six agent tools themselves, independent of the executor),
                  pg / pgmq / redis (storage and queues), s3 (S3-compatible object storage, file bytes), oidc (OpenID Connect login),
                  github (GitHub device flow login),
                  cryptobox / secret (encryption and credentials), filebytes (reading/writing attachment bytes)
  shared/gopkg/   **pure function helpers (no io)**: arkframe (reading Ark event frames), attachment, authzdoc, humanize, mdtitle,
                  resource, selfurl, requestid, xlog
web/              frontend Vite + React 19 + TS + Tailwind 4 + HeroUI 3
cli/              npm wrapper for the ama command (prebuilt binaries)
sandbox-image/    image for Docker session containers
deploy/           default config inside the amad image
repos/            read-only clones of third-party projects used as references (git-ignored, see Working rules)
docs/
  DESIGN.md       frontend design doc: information architecture, data model, routes, how events are shown
  guides/         development guides by task, see the index table above
scripts/
  brand-icons/    brand image → favicon/PWA icons + vector paths for the in-app logo
  model-icons/    model vendor logos → inline vector paths
  export-seed/    export the frontend seed.ts to seed.json
  setup-ark-agent/  scripts to set up agents and skills on Ark
  gen-schema-outline.py  regenerate the "table overview" at the top of each .sql file
```

Every business module and integration package under `server/` has its own README, which is the primary source
for that package; `docs/guides/` only pulls out "what you must know before you start".

## Common commands

**Run the whole stack (at the repo root)**:

```bash
./scripts/init-env.sh            # generate .env (passwords and keys are random; compose refuses to start if any is missing)
docker compose up -d --build     # PG + Redis + object storage (RustFS) + session image + amad, open http://localhost:9999
```

**Local development**: build frontend and backend in one step and start a single process:

```bash
docker compose up -d postgres redis s3   # start only the dependencies (PG port 15432, Redis 16379, object storage 19000)
./dev.sh                     # build frontend → embed into backend → restart, listens on 0.0.0.0:5273
./dev.sh --skip-web          # only Go code changed, skip the frontend build
./dev.sh --stop              # stop
./dev.sh --logs              # follow logs
```

**Running it again restarts it.** The frontend build output is embedded into the binary with `go:embed`
(`server/httpd/static.go`), so locally there is one process, one port, and naturally the same origin.
**There is no HMR; frontend changes require running `./dev.sh` again.**

⚠️ **"One process" is only how it looks locally; production runs several instances** (see "This is a distributed service" above).
Working in a single local process is not evidence that "the implementation is correct": in-process state is always right here.

⚠️ **Before starting a new process, `dev.sh` cleans up old processes by port** (first `kill`, then `kill -9` if
it's still there after 0.5 seconds). It doesn't check whose process it is. When running several copies on one
machine (for example several git worktrees), give each its own port, and pass the same `PORT=` to `--stop` too:

```bash
PORT=5301 ./dev.sh --skip-web
PORT=5301 ./dev.sh --stop
```

An empty `PORT=` is the same as not setting it (`${PORT:-5273}` also falls back to 5273 for an empty string).

Backend (under `server/`):

```bash
go build ./...
go vet ./...
gofmt -l .                  # empty output means formatting is correct
go test -race ./...         # no credentials needed
```

⚠️ **Tests that touch the database `t.Skip` as a whole group when they can't connect to PG, and a skip shows as green.**
If you changed DDL or SQL in `*_pg.go`, you have to start a PG and run them again before it counts as tested.
They connect to `127.0.0.1:15432` by default (database/user is `ama`, i.e.
`docker compose up -d postgres`). ⚠️ The compose PG password is randomly generated into `.env`, **so you must pass
`AMA_TEST_PG_PASSWORD=$(grep ^AMA_PG_PASSWORD= .env | cut -d= -f2-)`**, otherwise authentication fails and the whole group is skipped and shows as green. You can use `AMA_TEST_PG_HOST` / `AMA_TEST_PG_PORT` /
`AMA_TEST_PG_USER` / `AMA_TEST_PG_PASSWORD` / `AMA_TEST_PG_DATABASE` to point somewhere else.
The tests create a temporary schema and load all DDL into it, **so don't point them at a database with real data**.

Frontend (under `web/`):

```bash
npm install
npm run build                # check-icons && tsc -b && vite build
npm run typecheck
npm run check-icons          # icon check: every icon used exists and is registered (works offline)
npm run e2e                  # Playwright, uses the system Chrome
./scripts/dev.sh             # start vite alone (has HMR, but no backend; day to day use the root dev.sh)
```

## Architecture notes

**Data lives in PostgreSQL.** Both `core.Store` and `feishu.Store` have a PG implementation, which is the default
(`AMA_STORE=mem` falls back to memory, only suitable for a single local process). Seed data is installed only into an
empty database; restarts don't wipe data.

**File bytes live in object storage, metadata in PG.** The bytes of attachments, skill files, skill packages and personal
files are stored only in S3-compatible object storage (`shared/infra/s3`, RustFS in compose); PG stores only metadata such as
bucket + key, **never any bytes**. With the PG store and no `s3.bucket` configured, amad refuses to start; it does not fall back to writing bytes into the database.
⚠️ **A presigned link has the Host signed into it**, so a link only works against the address it was signed for: browsers /
bring-your-own machines use `s3.publicEndpoint`, Docker session containers use `s3.sandboxEndpoint`, and amad's own reads and writes use `s3.endpoint`.
The symptoms of signing for the wrong one don't point to config (browser download 403, only SKILL.md left of a skill inside the container); see `server/shared/infra/s3/README.md`.

**Conversations run on self_hosted, and tools run in our own executor.** The three endpoints behind the chat page
(create session / send event / SSE subscribe) drive Managed Agents with environment type `self_hosted`:
Ark only does model inference and puts tool calls on the work queue; a long-running worker inside amad picks them up, sends
them to the executor, then returns the result to Ark as a `user.tool_result` event.

**Do not use the `cloud` environment.** With it, tools run in Ark's managed sandbox and the execution environment is out of our hands.
`EnsureEnvironment` reads the environment back after creating it and checks its type; if it's not self_hosted it fails right away.

Code: `core/chat.go` (sessions) + `core/worker.go` (worker pool)
+ `shared/infra/dockertools/` (Docker executor) + `shared/infra/shelltools/` (the six tools)
+ `service/host/` (bring-your-own-machine executor). Pitfalls are recorded in `server/core/README.md`.

**All six tools (bash/read/write/edit/glob/grep) run in the same execution environment** and share one file system.
Swapping only bash leads to "a file written by bash can't be read by read". That mismatch raises no error; it just makes the agent retry over and over.
The Docker executor's workspace is a named volume per session; if the container is deleted it is recreated automatically and the files are still there.

**A container is created only the first time a tool is really called, and paused when idle after a turn ends.** After `end_turn`,
once idle for the SDK's `MaxIdle` (60 seconds by default), it is handled per `docker.idleAction` (default `pause`, or `stop` / `none`),
and resumed when the next message really calls a tool; plain text replies don't touch the container. The container name is derived from the
session id, and when several instances race to create it, the daemon's 409 decides; the session-to-container mapping is stored in PG
(`run_sandbox_sessions`). The cleanup check grabs a Redis lease, deletes containers idle longer than `containerTTL` (24h), and deletes
workspaces idle longer than `workspaceTTL` (7d).

The worker must be able to reach the Docker daemon (default `unix:///var/run/docker.sock`, or `DOCKER_HOST` /
`docker.host`); the session image defaults to `ama-sandbox:latest` (`sandbox-image/Dockerfile`).
If it can't connect, the service still starts, but **the agent gets an error as soon as it calls a tool**; the startup log says clearly whether the Docker executor was wired up.
⚠️ **Access to docker.sock equals root on the host**; for deployment and isolation see
[docs/guides/docker-executor.md](docs/guides/docker-executor.md).

**Config**: one `config.Config` holds all config (pg / redis / server / auth / apiKey / ark / docker /
worker). It comes from a single YAML file (set by `AMA_CONFIG`, default `config.yaml`) plus environment variables,
with priority **environment variables > file > defaults**. There is no `os.Getenv` business config anywhere in `cmd/amad`.
See [server/shared/infra/config/README.md](server/shared/infra/config/README.md).

**There are three ways to log in: local accounts, generic OIDC, and GitHub**. The config keys `auth.local` / `auth.oidc` / `auth.github` decide which are on
(`auth` package + `shared/infra/oidc`):

- Local accounts: `POST /api/auth/login` / `POST /api/auth/signup`, password hashes stored with bcrypt (`auth_local_accounts`).
  With `auth.local.allowSignup=false`, **signing up the first account is still allowed** (when there are no users at all in the database, it becomes the platform admin);
  after that the platform admin creates accounts with `POST /api/admin/users`. Password login is rate-limited per email, with the counters in PG (`auth_login_failures`).
- OIDC: `GET /api/auth/oidc/login` → IdP → `GET /api/auth/oidc/callback`, Authorization Code + PKCE +
  state + nonce, all three stored in PG (`auth_oidc_states`, since the callback may land on another instance). The email comes from the ID token's `email`,
  and `email_verified` is required by default.
- GitHub: OAuth device flow (`POST /api/auth/github/device` → the user enters the code on github.com → the browser polls
  `POST /api/auth/github/poll`). Only a client id is needed (if not set, it defaults to the GitHub CLI's public client id, see `github.DefaultClientID`). The device_code and polling interval are stored in PG (`auth_github_flows`),
  and the flow id must also come with an HttpOnly binding cookie before it can be polled; **if the allow lists are all empty, nobody gets in**. See `auth/github.go`.
- `GET /api/auth/providers` tells the frontend which methods are on. If none is on, the startup log shows an ERROR and people can't log in to the web UI.

**The lowercase email is the user's unique ID (Actor)**. Apart from the login endpoints under `/api/auth/` and `/healthz`, every endpoint requires login.
The credential is a random token in an HttpOnly cookie (the database stores only its sha256, so logging out really revokes it),
and identity travels in `context`, not in request headers (request headers can be changed by the caller; only the server can write into context).
e2e / CI / scripts use a fixed API key (`AMA_API_KEY` + `AMA_API_KEY_EMAIL`, enabled only when both are set;
don't set them in production). Users can also create their own API keys in settings (`amak_` prefix), with permissions exactly those of the user.
⚠️ The API key goes in the `Authorization` header, **never in command-line arguments**. The `ama` CLI deliberately has no `--token` flag,
because the command line is recorded as-is in `RunEvent` forever.

**Authorization**: the `authz` package decides permissions by **workspace member role**: owner/admin can do everything, member can read and write, viewer can only read,
**non-members always get 403**, and the workspace list returns only the workspaces you belong to. ⚠️ **Every new HTTP route must declare its permission in
`server/authz/matrix.go`, otherwise startup panics** (a missing declaration would silently let requests through, a failure with zero errors).
Four system-driven paths (worker pool checks, Feishu inbound, WeChat inbound, scheduled jobs) **have no identity and skip authorization on purpose**.
Don't add auth to them: doing so makes the bot silently stop replying and tool calls hang. Read
[server/authz/README.md](server/authz/README.md) before you start.

**The chat page** (`/w/:key/chat`) reuses Run/RunEvent: a conversation is just a run with `trigger: "manual"`,
so it shows up in run history automatically and can be continued from history. Real-time output uses the character-by-character
`event_deltas` from Ark's SSE; delta frames are deliberately kept out of the `RunEvent` union (Ark states clearly that they are not
stored in event history). See `docs/DESIGN.md`.

## Creating tables: spread fields into columns, use a link table for one-to-many

**By default spread fields into columns; don't stuff them into a `doc` / `extra` jsonb.** There are only two questions to decide with:

> **1. Can you list all the fields of this thing?** (check in order; the first match wins)
>
> 1. **Is it one-to-many?** → **Create a link table**, and stop there. **PG cannot put a unique constraint on jsonb array elements**,
>    so in a doc array you can't even dedupe. ⚠️ This rule comes before everything below,
>    including "the keys are chosen by the user": user-defined env vars / tags **have a fixed shape** (two columns, key and value).
>    The user picks the values, not the shape, so it's a link table + `unique(owner, key)`, not jsonb.
> 2. **You can list them all** → make columns. Make as many columns as there are fields; there is no upper limit. "Too many fields" and "we may add fields later"
>    are not reasons (adding a column can ship in the same release as the code; only dropping a column is the hard direction).
> 3. **You can't list them all, and someone else decides the shape** (raw structures returned by a third party, user-defined key-value data) → only then use jsonb.
>    ⚠️ **"Byte-for-byte faithful" needs `text`/`bytea`**: jsonb reorders keys and strips whitespace, so it is exactly not faithful
>    (precedent: `run_events.raw`).
>
> **2. (Ask again when you want jsonb) Does any query need to filter, sort,
> group or dedupe by some field inside it?** If yes, even just one → **that field must be a column**. Only if none does, and it is a
> blob that is stored and read back as a whole (a variable number of detail rows, a snapshot to replay as one frame), can it go into jsonb as a whole,
> and **the rest of this table's fields must still be real columns**.

⚠️ **"It goes into the HTTP response" is not a reason to use doc.** The `issues` / `suggestions` areas are all real columns,
without a single doc, and they go into responses just the same.

⚠️ **When adding a field to a table that already has a doc, add it as a column too**; don't put one more key into `agents.doc` / `runs.doc`
(that is the default path with zero DDL and zero errors, and the rule is easiest to break right there).
**A missing column is a compile error; a missing doc field is data loss you discover weeks later.**

⚠️ **This rule has an automated gate**: adding a `doc jsonb` column, or adding jsonb to a table not on the allow list,
makes `go test ./repository/schema/` fail, and the error message includes the rules.

Right now 9 of the 52 tables have a doc column (for historical reasons; not required to change, and when migrated they move toward all-columns). New tables must follow the rule.
For the full rules and how to do it, see [server/repository/schema/README.md](server/repository/schema/README.md).

## Timestamps: unit and format

**Timestamps produced by this project are always ISO 8601 strings with exactly three decimal places (milliseconds), in UTC,
ending with a literal Z**: `2026-07-26T10:00:00.000Z`. Generate them with `apimodel.NowISO()`.

### Pick the column type by use, one of two; don't go by feel

| Use | Column type | Precedent |
| --- | --- | --- |
| **Goes into the HTTP response** and is sorted as a string | `text` (opaque ISO string) | `runs.started_at`, `issues.created_at` |
| **Doesn't go into responses**, used by the database for comparing/sorting | `timestamptz` | `schedules.next_run_at`, `workspaces.created_at`, `auth_sessions.created_at` |

The test is "will the frontend compare this value as a string". If yes, it must be text with a fixed three decimal places;
if no (it's only used in `order by` / `where`), use `timestamptz` and let the database be the only clock.

**The same concept can exist on both sides**: the `created_at` column of `workspaces` is `timestamptz` (for sorting),
while `createdAt` in `doc` is still an ISO string (for the response); both are written in the same write.

### Don't use `time.RFC3339`

Both `time.RFC3339` and `RFC3339Nano` **drop the `.000`**, so the same moment ends up written two ways in the database.
This isn't about looks: `listRuns` sorts by `startedAt` in descending **string** order,
and `.`(0x2E) < `Z`(0x5A), so mixing the two shapes **directly changes the sort result**.

### ⚠️ Unit mix-ups: check every timestamp that comes from outside

Inside this project there is only one unit (millisecond ISO strings), **but the external systems we talk to are not consistent, and two
endpoints from the same vendor can differ**: JWT `exp` / `iat` are seconds (per the spec), while many open platform APIs return
`expire_at` in milliseconds. Parsing milliseconds as seconds gives an expiry tens of thousands of years away, so the token "never expires",
then silently gets 401 when it really expires, and the "still plenty of time" check in the code never fires once.

**Only the docs can tell you; don't guess.** The test is simple: parse the number as seconds; if you get a year very far from now
(tens of thousands of years ahead), it's actually milliseconds.

## Code that needs config to work must ship together with the config

When you add a config item, the same change must also fill in `server/config.example.yaml` (with a comment on each item),
the environment variable mapping in `server/shared/infra/config`, and if needed `.env.example` and `docker-compose.yml`.
Committing only the code that reads the config is shipping a feature that doesn't work, and such half-done work has three things in common:

1. **Zero errors**: the service starts as usual and health checks stay green; only that one feature silently doesn't work;
2. **The error doesn't point to config**: the symptom shows up at the far end (for example "running `ama` in the container says there are no credentials"),
   and that message points at the CLI, not at "a key is missing from the config";
3. **The knowledge is lost at handover**: you know one step is missing, but the next person only sees "it's right there in the code, it just doesn't work".

When verifying, pick a signal **that can prove you wrong**:

- **Config is read only at startup.** During a rolling deploy, "instances that started before the config arrived" still report missing config.
  The test is "do instances that started after the config arrived still report it".
- **Don't trust logs that only check that an object is non-nil.** In dependency injection that object is always non-nil, so the startup log says
  "wired up" while the feature actually doesn't work. Ask instead "does this feature itself report that it is available" (`XxxEnabled()`),
  not "is the pointer there".

## Conventions

**Use plain words, not fancy ones.** Package names, type names, function names, comments, docs, and conclusions written for users
all use the most ordinary way of saying it.

The test is **"can someone who just joined this repository understand it at a glance, without a dictionary and without asking anyone"**. If not, change it.

This is not a style preference. The problem with abstract nouns is that **they look like they explain something, but they don't**:

- Words like `contract` / `entity` / `descriptor` / `provisioning` / `orchestration` force the reader to guess
  what they mean in this repository, and a wrong guess doesn't trigger any error;
  it only leads the next person to put the wrong thing in. `model/apimodel` (used for HTTP) needs not a single guess,
  while `model/contract` ("contract"? between whom?) does.
- Be especially careful with package names: they appear at **every call site** (`apimodel.Agent` gets written thousands of times),
  so the cost of a bad choice grows with the number of references, and renaming means touching thousands of places.
- The same goes for prose: **do not write words like "converge", "align", "semantics", "paradigm" to describe a concrete change; say what you actually did.**
  Say "changed the three places to use the same code", not "converged the implementation"; say "both sides give the same
  answer", not "aligned the semantics".

⚠️ **There is only one exception: the word is already the name of something in an external system.** `OAuth`, `jsonb`,
`timestamptz`, `ArkBinding`, `SkillsFingerprint`, `idempotency key` are used as-is. Replacing them with "our own words"
makes the docs unsearchable and breaks the link to upstream. The test is "can you find it in the official docs".

**Go**: standard `gofmt`. **Code comments are always in English** (every language: Go / TS / SQL / shell / YAML, etc.), and commit messages are in English too (see "Commits and pull requests"). Log lines, error messages and UI copy are not comments and keep their existing style. `docs/` and the package READMEs are still partly in Chinese; translate them to English when you substantially change them (see Working rules).

**Frontend**: read `docs/DESIGN.md` before writing `web/src`. The `@` alias points to `web/src`.
TypeScript is in strict mode with `noUnusedLocals` / `noUnusedParameters` on,
so unused variables are compile errors.

**If you change an HTTP endpoint you must regenerate OpenAPI, and this is enforced automatically.**

```bash
cd server && go run ./cmd/gen-openapi     # regenerate openapi/openapi.json
```

The content of `GET /api/openapi.json` is **generated from the route permission table in `authz/matrix.go`**, not hand-written
(hand-written specs always drift, and an agent holding an outdated spec keeps calling endpoints wrongly, with errors that don't point to the real cause).
The output `server/openapi/openapi.json` is checked in, so commit it along with your change.

What happens if you forget: **`go test ./openapi/` fails**, and the error message includes the fix command. The whole loop is

```
new route → must be added to matrix (if missing, the service panics on startup)
        → spec changes automatically (generated from matrix)
        → the up-to-date check makes sure the spec was regenerated and committed (TestSpecFileIsUpToDate)
```

**The same matrix also generates a human-readable "role permission table"** (the collapsible section at the bottom of the members page):

```bash
cd server && go run ./cmd/gen-authz-capabilities   # output: web/src/data/roleCapabilities.ts
```

Same reason: hand-written permission notes always drift, and **an outdated permission note is worse than none**
(users read it, think they can do something, and get a 403).
If a route isn't categorized or you forget to regenerate: **`go test ./shared/gopkg/authzdoc/` fails**, and the error includes the fix command.
⚠️ **owner and admin in that table are always exactly the same** (they are the same level in `levelOf`); giving owner extra checkmarks is wrong.
See [server/authz/README.md](server/authz/README.md).

**Keep temp files out of the repository**: probe scripts and temporary configs go in `/tmp`; vite/playwright configs
that must live in the package directory to resolve dependencies are deleted right after use.

## Commits and pull requests

- Branch off `main`, open a GitHub pull request, and don't push straight to `main`.
- **Commit messages must be in English** (both title and body), starting with `type(scope): summary`
  (e.g. `fix(feishu): ...`, `feat(web): ...`). This is a public repository, and the commit history must be readable by contributors
  who don't read Chinese; code comments are in English too. `docs/` and the package READMEs are still partly in Chinese; translate them to English when you substantially change them (see Working rules).
- **Write the pitfalls you hit into the commit body or into the nearest README / the matching guide under `docs/guides/`**;
  don't leave them only in the PR discussion.
- Before opening a PR, run locally: backend `gofmt -l .` (empty) + `go build ./...` + `go vet ./...` +
  `go test -race ./...` (with PG if you touched SQL); frontend `npm run typecheck` + `npm run build`.
  If you changed routes, include the regenerated `openapi.json` and `roleCapabilities.ts`.
- The PR description says what changed, why, and how it was verified.
