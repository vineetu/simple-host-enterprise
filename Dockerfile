# Two stages: a full toolchain to build, and a distroless image that holds the
# binary and nothing else. No shell, no package manager, no root; the same
# image runs the server and, as an init container, `simple-host migrate`.
# --platform=$BUILDPLATFORM keeps the toolchain native and cross-compiles to the
# target instead of emulating it. The binary is CGO-free, so Go does this for
# free; without it, building an amd64 image on an arm64 machine runs the whole
# Go toolchain under QEMU and takes minutes rather than seconds.
# Base images are pinned by digest so a rebuild uses the same bytes; Dependabot
# (.github/dependabot.yml) proposes digest bumps. golang:1.27.1 and
# distroless static:nonroot as of 2026-10-02.
FROM --platform=$BUILDPLATFORM golang:1.27.1@sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190 AS build
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

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
# ca-certificates ships in distroless; the database CA is mounted, not baked.
COPY --from=build /out/simple-host /simple-host
USER 65532:65532
EXPOSE 8080 8081 9090
ENTRYPOINT ["/simple-host"]
