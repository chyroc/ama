# Running the Docker executor on one big Volcengine ECS

This guide sets up the Docker executor so that **many sessions share one large ECS instance while
each session stays isolated**, with shared business data kept on an EBS disk. It builds on
[docker-executor.md](docker-executor.md); read that first, especially the security section.

What each layer gives you:

| Layer | Isolation it provides |
| --- | --- |
| One container + one workspace volume per Ark session (built in) | Separate filesystem, processes and workspace per session |
| `docker.runtime: runsc` (gVisor) | Each session gets its own user-space kernel instead of sharing the host kernel |
| `docker.cpus` / `memory` / `pidsLimit` | One session cannot starve the others |
| `docker.capDrop`, `no-new-privileges` (built in) | Less that root inside the container can do |
| `docker.dataMounts` with `readOnly: true` | Shared data is visible to every session but cannot be changed by them |
| `docker.network` | Controls what sessions can reach |

## 1. Prepare the instance

- Pick an instance size from expected concurrency: roughly `concurrent sessions x docker.memory`
  of RAM, plus headroom for amad and the page cache.
- Attach two EBS data disks (or one, split into directories):
  - one for Docker's data root (images, containers, **workspace volumes**), so workspaces survive
    instance re-creation and do not fill the system disk;
  - one for business data that sessions read.
- Format and mount them, and add them to `/etc/fstab` with `nofail`:

```bash
mkfs.ext4 /dev/vdb && mkdir -p /mnt/docker && mount /dev/vdb /mnt/docker
mkfs.ext4 /dev/vdc && mkdir -p /mnt/data   && mount /dev/vdc /mnt/data
```

Device names differ by instance type; check with `lsblk`.

## 2. Install Docker and gVisor

Install Docker Engine, then gVisor (see https://gvisor.dev/docs/user_guide/install/). Register
`runsc` and move the data root onto EBS in `/etc/docker/daemon.json`:

```json
{
  "data-root": "/mnt/docker",
  "runtimes": {
    "runsc": { "path": "/usr/bin/runsc", "runtimeArgs": ["--network=host"] }
  }
}
```

```bash
systemctl restart docker
docker run --rm --runtime=runsc hello-world   # must print the greeting
```

Notes on gVisor:

- **`--network=host` is needed when sessions join a user-defined network** (the compose setup uses
  `ama-net`). With gVisor's own network stack, Docker's embedded DNS (127.0.0.11) does not resolve,
  so session containers cannot reach `amad`, `ama-s3` or any host name. With `--network=host` the
  container still has its own network namespace; only the network syscalls go to the host kernel
  instead of gVisor's netstack, which is a smaller isolation gain for networking.
- `docker pause` / `unpause` work under runsc, so the default `docker.idleAction: pause` is fine;
  if you see problems, switch to `stop`.
- Some syscalls are not implemented; heavy native tooling may behave differently than on runc.
  Try the agent's typical commands (git, pip, node) inside a runsc container before going live.
- File I/O on bind mounts is slower than on runc. For large read-heavy datasets that is usually
  acceptable; benchmark if it matters.

Block the instance metadata service from containers, so a session cannot read instance
credentials (`100.96.0.96` on Volcengine; the host itself keeps access):

```bash
iptables -I DOCKER-USER -d 100.96.0.96/32 -j DROP
```

Run it from a oneshot systemd unit with `After=docker.service` / `PartOf=docker.service` so it
survives Docker restarts and reboots.

### Installing from inside China

Docker Hub, GitHub and proxy.golang.org may be unreachable or slow from the instance:

- Docker 29 (the `docker.io` package on Ubuntu 22.04) needs ama with Docker API version
  negotiation; older ama builds fail with "client version 1.41 is too old".
- Add a registry mirror to `daemon.json`, for example `"registry-mirrors": ["https://docker.m.daocloud.io"]`.
- gVisor's apt repository at `storage.googleapis.com` can be very slow. Downloading
  `pool/<version>/binary-amd64/runsc.deb` elsewhere, checking its SHA256 against the repository's
  `Packages` index, and installing it with `apt-get install ./runsc.deb` works.
- Pass build args through `docker-compose.override.yml`:

```yaml
services:
  sandbox-image:
    build:
      args: {GOPROXY: "https://goproxy.cn,direct", APT_MIRROR: "https://mirrors.volces.com"}
  amad:
    build:
      args: {GOPROXY: "https://goproxy.cn,direct", APT_MIRROR: "https://mirrors.volces.com"}
```

## 3. Put business data on EBS

```bash
mkdir -p /mnt/data/marketing
# copy or sync the data into it
chmod -R a+rX /mnt/data/marketing
```

Session commands run as the image's default user; make sure that user can read the files.

## 4. Configure amad

```yaml
docker:
  image: ama-sandbox:latest
  cpus: 2
  memory: 4g
  pidsLimit: 512
  runtime: runsc
  # capDrop: [ALL]   # only if the agent never needs root (apt install, chown) in the container
  dataMounts:
    - {source: /mnt/data/marketing, target: /data/marketing}   # readOnly defaults to true
```

`source` is a path **on the Docker host**. amad runs on the same ECS here, so it is the same
machine; if you point `docker.host` at another instance, the directory must exist there.

Tell the agent where the data is, for example in its system prompt: "Marketing data is under
/data/marketing (read-only). Write results to the current working directory."

## 5. Run amad as a service

Build amad, then run it under systemd so it restarts on failure and on reboot:

```ini
[Unit]
Description=amad
After=network-online.target docker.service
Requires=docker.service

[Service]
Environment=AMA_CONFIG=/path/to/config.yaml
ExecStart=/path/to/amad
Restart=always
User=ama
SupplementaryGroups=docker

[Install]
WantedBy=multi-user.target
```

Membership in the `docker` group equals root on this host (see the security section of
[docker-executor.md](docker-executor.md)); do not expose amad's admin routes publicly.

## 6. Verify

1. Start a chat, ask the agent to run `ls /data/marketing && touch /data/marketing/x`: the listing
   works and the write fails with "Read-only file system".
2. `docker inspect <session container> --format '{{.HostConfig.Runtime}}'` prints `runsc`.
3. Inside the session, `dmesg | head -1` mentions gVisor instead of the host kernel.
4. Two sessions do not see each other's workspace files.
