# companion — sidecar. Our Go binary + buildctl (copied from the pinned buildkit
# image) so /readyz and the inode backstop can call `buildctl ... debug workers / prune`.
# Keep BUILDKIT_VERSION aligned with the daemon image (buildd --buildkit-image) to avoid
# buildctl/buildkitd version skew.
# renovate: datasource=docker depName=moby/buildkit
ARG BUILDKIT_VERSION=v0.31.1
FROM golang:1.26@sha256:32c0e6e5c4f6707717051091b4d0b077464a679eaab563e11474efc5328e2aa5 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/companion ./cmd/companion

# Intentionally tag-pinned (not digest): BUILDKIT_VERSION must track the daemon image
# (buildd --buildkit-image) so buildctl and buildkitd stay in lockstep — a digest here would
# freeze that coupling.
FROM moby/buildkit:${BUILDKIT_VERSION} AS buildkit

FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
COPY --from=build /out/companion /companion
COPY --from=buildkit /usr/bin/buildctl /usr/bin/buildctl
USER 65532:65532
ENTRYPOINT ["/companion"]
