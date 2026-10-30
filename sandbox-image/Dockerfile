# Image for the session container: the agent's six tools (bash/read/write/edit/glob/grep) all
# run inside it.
#
# Build (from the repo root; the context must include server/ so the ama CLI can be built):
#
#   docker build -t ama-sandbox:latest -f sandbox-image/Dockerfile .
#
# The image has only four hard requirements: bash, coreutils (including timeout; command
# timeouts rely on it to kill processes), GNU findutils (the web page's "view files" uses
# find -printf), and grep. Everything else is there to help the agent do its work.
# If you swap in your own image, it just needs these four; point docker.image at it.

# ---- 1. Build the ama CLI (the command the agent uses inside the container to operate the AMA platform) ----
FROM golang:1.24-bookworm AS cli
WORKDIR /src/server
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN go build -trimpath -ldflags='-s -w' -o /out/ama ./cmd/ama-cli

# ---- 2. Runtime ----
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash coreutils findutils grep sed gawk procps less \
      ca-certificates curl wget git openssh-client \
      python3 python3-pip python3-venv \
      jq ripgrep unzip zip xz-utils vim-tiny \
 && rm -rf /var/lib/apt/lists/*
COPY --from=cli /out/ama /usr/local/bin/ama
# The workspace volume is mounted here (the default value of docker.workdir).
WORKDIR /workspace
# amad keeps the container alive with its own entrypoint (see dockertools.containerConfig), so
# no CMD is needed here; we set one so a manual `docker run -it ama-sandbox` for debugging gets a shell.
CMD ["bash", "-l"]
