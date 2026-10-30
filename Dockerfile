# Image for the AMA server (amad): the frontend build output is go:embed-ed into the binary; one process, one port.
#
#   docker build -t ama:latest .
#
# At runtime it needs PostgreSQL (required), Redis (optional, for leader election across instances), and a Docker daemon (session containers).
# A ready-to-run setup is in docker-compose.yml.

# ---- 1. Frontend ----
FROM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
# check-models cross-checks server/core/seed.json, so it has to be copied in.
COPY server/core/seed.json /src/server/core/seed.json
RUN npm run build

# ---- 2. Backend (embed the frontend output) ----
FROM golang:1.24-bookworm AS server
WORKDIR /src/server
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
# server/httpd/static is the go:embed directory (see server/httpd/static.go); this is the same thing dev.sh does.
COPY --from=web /src/web/dist/ ./httpd/static/
RUN go build -trimpath -ldflags='-s -w' -o /out/amad ./cmd/amad

# ---- 3. Runtime ----
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*
COPY --from=server /out/amad /usr/local/bin/amad
# A config with only the minimum required fields; everything else comes from environment variables (see deploy/config.yaml).
# To use a full config, mount your own file and change AMA_CONFIG.
COPY deploy/config.yaml /etc/ama/config.yaml
ENV AMA_CONFIG=/etc/ama/config.yaml \
    AMA_ADDR=:9999
EXPOSE 9999
ENTRYPOINT ["/usr/local/bin/amad"]
