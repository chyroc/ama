# Deployment

English | [中文](deployment.zh-CN.md)

## Deploy with docker compose

You need Docker (with the compose plugin) and an API key for a Volcengine Ark account.

```bash
./scripts/init-env.sh    # writes .env with random passwords for PG / Redis / object storage and two encryption keys
docker compose up -d --build
docker compose ps        # wait until postgres / redis / s3 are healthy and amad is up
```

Then:

1. Open <http://localhost:9999> and sign up. The first account becomes the platform admin. After that,
   self sign-up is off by default and the platform admin creates accounts with `POST /api/admin/users`.
   To allow sign-up, set `AMA_AUTH_LOCAL_ALLOW_SIGNUP=true` in `.env` and run `docker compose up -d`.
2. Enter your Ark API key under "Ark credentials" in the workspace settings (or add an account-level
   key that every workspace can reuse). amad creates the `self_hosted` Environment the first time a
   session starts; you don't need to create it by hand.
3. Create an agent and start chatting. amad starts the session container through Docker only when the
   agent calls a tool for the first time (image `ama-sandbox:latest`, already built by compose from
   `sandbox-image/Dockerfile`).

compose runs five services: `postgres` (host port 15432), `redis` (16379), `s3` (RustFS,
`${AMA_S3_PORT:-19000}`), `sandbox-image` (only builds the session image and exits), and `amad`
(`${AMA_PORT:-9999}`).

**Ports and passwords**

- amad and s3 listen on all interfaces (the browser needs them). PG and Redis **bind to 127.0.0.1 only**,
  for `dev.sh` and debugging on the same machine; amad reaches them over the compose network.
- The six passwords/keys (`AMA_PG_PASSWORD`, `AMA_REDIS_PASSWORD`, `AMA_S3_ACCESS_KEY`,
  `AMA_S3_SECRET_KEY`, `ARK_SECRET_KEY`, `AMA_AUTH_MANAGED_KEY_SECRET`) **must all be set in `.env`**.
  compose refuses to start if any is missing, so there is no "forgot to change the default password".
- ⚠️ The PG password only takes effect when the data volume is first created. Changing
  `AMA_PG_PASSWORD` later does not change the password in the database and amad will fail to connect.
  Either run `ALTER USER ama PASSWORD '...'` in the database, or `docker compose down -v` to recreate it
  (this deletes the data).
- When accessing from another machine, set `AMA_PUBLIC_URL`, `AMA_TERMINAL_WS_ORIGINS` and
  `AMA_S3_PUBLIC_ENDPOINT` to the real address (they all default to localhost).

⚠️ **Presigned object storage URLs include the host in the signature**, so a URL only works with the
address it was signed for. Download links for personal files are signed for the browser
(`AMA_S3_PUBLIC_ENDPOINT`, default `http://localhost:19000`). If the browser is not on this machine,
change it to `http://<host IP or domain>:19000`, or downloads won't open. Session containers fetch skill
packages from a different address (`http://ama-s3:9000` on the container network); the two are not
interchangeable. Issue and suggestion attachments are not affected (they go through amad). See the `s3`
section of `server/config.example.yaml`.

⚠️ `AMA_PUBLIC_URL` is also injected into session containers as `AMA_BASE_URL` for the `ama` command.
The default `http://localhost:9999` points at the container itself, so `ama` inside the container
cannot reach amad. If agents should use `ama` in containers, set `AMA_PUBLIC_URL` to an address both the
browser and session containers can reach (a domain behind a reverse proxy, or the host's LAN IP).

## ⚠️ Docker executor security

compose mounts the host's `/var/run/docker.sock` into amad. **Access to docker.sock equals root on the
host** (it can start privileged containers and mount the host root). So:

- amad itself must be trusted. Don't expose it (especially the fixed API key and the debug routes enabled
  by `AMA_DEV_ROUTES`) to untrusted people.
- Session containers don't get the socket, but they share the host kernel. Container isolation is not
  VM isolation.
- For stronger isolation, run the daemon rootless or with userns-remap, use a runtime such as gVisor
  (runsc), or point amad at a remote daemon dedicated to session containers with `AMA_DOCKER_HOST`.

Details: [docs/guides/docker-executor.md](guides/docker-executor.md) (Chinese).
For running tools on your own machine, see [docs/guides/host-executor.md](guides/host-executor.md) (Chinese).

## Configuration

Configuration comes from a single YAML file (path set by `AMA_CONFIG`) plus environment variables.
Precedence: **environment variables > file > defaults**.

- [`server/config.example.yaml`](../server/config.example.yaml): every option with comments. For local
  development, copy it to `server/config.yaml`.
- [`deploy/config.yaml`](../deploy/config.yaml): the default config inside the image. It only sets the PG
  connection; everything else comes from environment variables.
- [`.env.example`](../.env.example): the environment variable template read by compose and `dev.sh`.
- Mapping between environment variables and fields:
  [`server/shared/infra/config/README.md`](../server/shared/infra/config/README.md).

Common environment variables:

| Variable | Purpose |
| --- | --- |
| `AMA_PG_HOST` / `AMA_PG_PORT` / `AMA_PG_DATABASE` / `AMA_PG_USER` / `AMA_PG_PASSWORD` / `AMA_PG_SSLMODE` | PostgreSQL connection |
| `AMA_REDIS_ADDR` / `AMA_REDIS_PASSWORD` | Redis (leave empty to disable the Feishu long connection) |
| `AMA_S3_ENDPOINT` / `AMA_S3_PUBLIC_ENDPOINT` / `AMA_S3_SANDBOX_ENDPOINT` | Object storage addresses: used by amad / signed for browsers and user machines / signed for session containers (the last two fall back to endpoint) |
| `AMA_S3_BUCKET` / `AMA_S3_REGION` / `AMA_S3_ACCESS_KEY` / `AMA_S3_SECRET_KEY` | Bucket and credentials (required with PG; amad refuses to start without them) |
| `AMA_S3_USE_PATH_STYLE` / `AMA_S3_CREATE_BUCKET` | Path-style addressing, create the bucket on startup (both default to true) |
| `AMA_PUBLIC_URL` | Public address: cookie `Secure` flag, OIDC callback, `AMA_BASE_URL` for `ama` in containers |
| `AMA_TERMINAL_WS_ORIGINS` | Page origins allowed to open the web terminal WebSocket |
| `ARK_SECRET_KEY` | Master key that encrypts Ark credentials (old ciphertext can't be decrypted after changing it) |
| `AMA_AUTH_LOCAL_ENABLED` / `AMA_AUTH_LOCAL_ALLOW_SIGNUP` | Local account login / self sign-up |
| `AMA_AUTH_OIDC_*` / `AMA_AUTH_ADMINS` | OIDC login / platform admins |
| `AMA_AUTH_GITHUB_*` | GitHub login (device flow), see below |
| `AMA_DOCKER_HOST` / `AMA_DOCKER_IMAGE` / `AMA_DOCKER_NETWORK` / `AMA_DOCKER_IDLE_ACTION` / `AMA_DOCKER_MEMORY` / `AMA_DOCKER_CPUS` | Docker executor |

### OIDC login

Any OpenID Connect provider that supports Authorization Code + PKCE works (Keycloak, Authentik, Google,
Azure AD, ...). Register `<AMA_PUBLIC_URL>/api/auth/oidc/callback` as the redirect_uri on the IdP:

```bash
# .env
AMA_PUBLIC_URL=https://ama.example.com
AMA_AUTH_OIDC_ISSUER=https://idp.example.com/realms/main
AMA_AUTH_OIDC_CLIENT_ID=ama
AMA_AUTH_OIDC_CLIENT_SECRET=...           # can be empty for public clients (PKCE only)
AMA_AUTH_OIDC_DISPLAY_NAME=Company SSO    # login button: "Sign in with Company SSO"
AMA_AUTH_ADMINS=alice@example.com         # the only way to make an OIDC user a platform admin
```

The user ID is the **lowercased email**: a local account and an OIDC login with the same email are the
same person. By default the ID token must have `email_verified=true`. Only turn on
`auth.oidc.allowUnverifiedEmail` if admins control the emails; otherwise anyone can claim someone
else's email and impersonate a member.

After editing `.env`, run `docker compose up -d` to recreate amad (config is only read at startup).

### GitHub login

Uses GitHub's OAuth device flow: the user clicks "Sign in with GitHub", the page shows a short code, the
user enters it at `github.com/login/device` and approves, and the page finishes signing in. **Only a
client id is needed: no client secret, no callback URL registered on GitHub.**

```bash
# .env
AMA_AUTH_GITHUB_ENABLED=true
AMA_AUTH_GITHUB_ALLOWED_USERS=alice,bob       # allowlist, see below
AMA_AUTH_GITHUB_ALLOWED_EMAIL_DOMAINS=example.com
AMA_AUTH_GITHUB_ALLOWED_ORGS=acme             # also requests read:org
AMA_AUTH_GITHUB_CLIENT_ID=Ov23li...           # optional, use your own OAuth App (see below)
AMA_AUTH_GITHUB_PROXY=http://172.17.0.1:8080  # optional proxy used only for github.com / api.github.com
```

**The default client id works, but replace it.** Without one, AMA uses the public GitHub CLI client id
`178c6fc778ccc68e1d6a` (from `internal/authflow/flow.go` in `github.com/cli/cli`; device flow needs no
secret, so it works as is). The cost: the GitHub consent page says "GitHub CLI" is asking for access
(misleading, and it makes device-code phishing more convincing), and GitHub can revoke it at any time,
which would break GitHub login entirely. amad logs a WARN about this at startup.

For production, create your own: GitHub → Settings → Developer settings → OAuth Apps → New OAuth App,
check "Enable Device Flow", fill Homepage / Callback URL with your site (device flow doesn't use the
callback), and put the client id in `AMA_AUTH_GITHUB_CLIENT_ID`.

**Access is denied by default**: when `ALLOWED_USERS` (GitHub usernames), `ALLOWED_EMAIL_DOMAINS`
(primary email domain) and `ALLOWED_ORGS` (full org members) are **all empty, no GitHub account can sign
in**. Set any of them and matching any one lets the user in. Anyone can create a GitHub account, so no
allowlist would open the platform to the whole world.

The email is the GitHub email that is **primary and verified**, lowercased, and it is the user ID: same
email as a local or OIDC account means the same person. Accounts without a verified primary email are
rejected. The GitHub access token is only used once to read the profile and email and is not stored. It
stays under "Authorized OAuth Apps" in the user's GitHub settings, where the user can revoke it.
