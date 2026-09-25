# Two stages: a full toolchain to build, and a distroless image that holds the
# binary and nothing else. No shell, no package manager, no root; the same
# image runs the server and, as an init container, `simple-host migrate`.
# --platform=$BUILDPLATFORM keeps the toolchain native and cross-compiles to the
# target instead of emulating it. The binary is CGO-free, so Go does this for
# free; without it, building an amd64 image on an arm64 machine runs the whole
# Go toolchain under QEMU and takes minutes rather than seconds.
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
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

FROM gcr.io/distroless/static:nonroot
# ca-certificates ships in distroless; the database CA is mounted, not baked.
COPY --from=build /out/simple-host /simple-host
USER 65532:65532
EXPOSE 8080 8081 9090
ENTRYPOINT ["/simple-host"]
