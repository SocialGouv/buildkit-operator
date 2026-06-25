# companion — sidecar. Our Go binary + buildctl (copied from the pinned buildkit
# image) so /readyz and the inode backstop can call `buildctl ... debug workers / prune`.
# Keep BUILDKIT_VERSION aligned with the daemon image (buildd --buildkit-image) to avoid
# buildctl/buildkitd version skew.
ARG BUILDKIT_VERSION=v0.22.0
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/companion ./cmd/companion

FROM moby/buildkit:${BUILDKIT_VERSION} AS buildkit

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/companion /companion
COPY --from=buildkit /usr/bin/buildctl /usr/bin/buildctl
USER 65532:65532
ENTRYPOINT ["/companion"]
