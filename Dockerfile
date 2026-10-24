# Two stages: a full toolchain to build, and a distroless image that holds the
# binary and nothing else. No shell, no package manager, no root; the same
# image runs the server and, as an init container, `simple-host migrate`.
# --platform=$BUILDPLATFORM keeps the toolchain native and cross-compiles to the
# target instead of emulating it. The binary is CGO-free, so Go does this for
# free; without it, building an amd64 image on an arm64 machine runs the whole
# Go toolchain under QEMU and takes minutes rather than seconds.
# Base images are pinned by digest so a rebuild uses the same bytes; Dependabot
# (.github/dependabot.yml) proposes digest bumps. golang:1.27.3 (go.mod's
# toolchain line) and distroless static:nonroot as of 2026-10-24.
FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/simple-host ./cmd/server

FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
# ca-certificates ships in distroless; the database CA is mounted, not baked.
COPY --from=build /out/simple-host /simple-host
USER 65532:65532
EXPOSE 8080 8081 9090
ENTRYPOINT ["/simple-host"]
