# companion — sidecar. Our Go binary + buildctl (copied from the pinned buildkit
# image) so /readyz and the inode backstop can call `buildctl ... debug workers / prune`.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/companion ./cmd/companion

FROM moby/buildkit:v0.18.2 AS buildkit

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/companion /companion
COPY --from=buildkit /usr/bin/buildctl /usr/bin/buildctl
USER 65532:65532
ENTRYPOINT ["/companion"]
