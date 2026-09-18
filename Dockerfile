# Two stages: a full toolchain to build, and a distroless image that holds the
# binary and nothing else. No shell, no package manager, no root; the same
# image runs the server and, as an init container, `simple-host migrate`.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/simple-host ./cmd/server

FROM gcr.io/distroless/static:nonroot
# ca-certificates ships in distroless; the database CA is mounted, not baked.
COPY --from=build /out/simple-host /simple-host
USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/simple-host"]
