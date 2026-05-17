# Multi-stage Dockerfile for scribe-web.
#
# Stage 1: build the React UI into cmd/server/web_dist
# Stage 2: build the Go binary (which embeds the UI via go:embed)
# Stage 3: build whisper.cpp from source
# Stage 4: minimal runtime image with ffmpeg + yt-dlp + whisper.cpp

# ---- 1. UI ----------------------------------------------------------
FROM node:22-bookworm-slim AS ui
WORKDIR /src
RUN npm config set registry https://registry.npmmirror.com && \
    npm install -g pnpm@9.15.5
COPY web/package.json web/pnpm-lock.yaml* ./web/
RUN cd web && pnpm config set registry https://registry.npmmirror.com && \
    (pnpm install --frozen-lockfile || pnpm install)
COPY . .
RUN cd web && pnpm run build

# ---- 2. Go server ---------------------------------------------------
FROM golang:1.23-bookworm AS gobuild
ENV GOPROXY=https://goproxy.cn,direct
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
RUN sed -i 's|deb.debian.org|mirrors.aliyun.com|g; s|security.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list.d/debian.sources 2>/dev/null || \
    sed -i 's|deb.debian.org|mirrors.aliyun.com|g; s|security.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list 2>/dev/null || true
RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates build-essential cmake && \
    rm -rf /var/lib/apt/lists/*
RUN (git clone --depth 1 --branch ${WHISPER_REF} https://gh-proxy.com/https://github.com/ggerganov/whisper.cpp.git /src/whisper || \
     git clone --depth 1 --branch ${WHISPER_REF} https://kkgithub.com/ggerganov/whisper.cpp.git /src/whisper || \
     git clone --depth 1 --branch ${WHISPER_REF} https://github.com/ggerganov/whisper.cpp.git /src/whisper) && \
    cmake -S /src/whisper -B /src/whisper/build \
        -DCMAKE_BUILD_TYPE=Release \
        -DBUILD_SHARED_LIBS=OFF && \
    cmake --build /src/whisper/build --target whisper-cli --config Release -j$(nproc)

# ---- 4. Runtime -----------------------------------------------------
FROM debian:bookworm-slim
RUN sed -i 's|deb.debian.org|mirrors.aliyun.com|g; s|security.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list.d/debian.sources 2>/dev/null || \
    sed -i 's|deb.debian.org|mirrors.aliyun.com|g; s|security.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list 2>/dev/null || true
RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg ca-certificates curl python3 python3-pip && \
    pip3 install --break-system-packages --no-cache-dir \
        -i https://mirrors.aliyun.com/pypi/simple/ --trusted-host mirrors.aliyun.com yt-dlp && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

COPY --from=whisper /src/whisper/build/bin/whisper-cli /usr/local/bin/whisper-cli
COPY --from=gobuild /out/scribe-web /usr/local/bin/scribe-web

ENV SCRIBE_DATA_DIR=/data \
    SCRIBE_ADDR=:8787

VOLUME ["/data"]
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/scribe-web"]
