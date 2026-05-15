# Multi-stage Dockerfile for scribe-web.
#
# Stage 1: build the React UI into cmd/server/web_dist
# Stage 2: build the Go binary (which embeds the UI via go:embed)
# Stage 3: minimal runtime image with ffmpeg + yt-dlp + whisper.cpp

# ---- 1. UI ----------------------------------------------------------
FROM node:20-bookworm-slim AS ui
WORKDIR /src
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml* ./web/
RUN cd web && pnpm install --frozen-lockfile || pnpm install
COPY . .
RUN cd web && pnpm run build

# ---- 2. Go server ---------------------------------------------------
FROM golang:1.23-bookworm AS gobuild
WORKDIR /src
COPY go.mod ./
RUN go mod download || true
COPY . .
COPY --from=ui /src/cmd/server/web_dist ./cmd/server/web_dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
    -o /out/scribe-web ./cmd/server

# ---- 3. whisper.cpp -------------------------------------------------
# Build whisper-cli from source so we ship a known-good static binary.
FROM debian:bookworm-slim AS whisper
ARG WHISPER_REF=master
RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates build-essential cmake && \
    rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch ${WHISPER_REF} https://github.com/ggerganov/whisper.cpp.git /src/whisper && \
    cmake -S /src/whisper -B /src/whisper/build \
        -DCMAKE_BUILD_TYPE=Release \
        -DBUILD_SHARED_LIBS=OFF && \
    cmake --build /src/whisper/build --target whisper-cli --config Release -j

# ---- 4. Runtime -----------------------------------------------------
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg ca-certificates curl python3 python3-pip && \
    pip3 install --break-system-packages --no-cache-dir yt-dlp && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

COPY --from=whisper /src/whisper/build/bin/whisper-cli /usr/local/bin/whisper-cli
COPY --from=gobuild /out/scribe-web /usr/local/bin/scribe-web

ENV SCRIBE_DATA_DIR=/data \
    SCRIBE_ADDR=:8787

VOLUME ["/data"]
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/scribe-web"]
