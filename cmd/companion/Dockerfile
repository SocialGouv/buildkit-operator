# companion — sidecar. Our Go binary + buildctl (copied from the pinned buildkit
# image) so /readyz and the inode backstop can call `buildctl ... debug workers / prune`.
# Keep BUILDKIT_VERSION aligned with the daemon image (buildd --buildkit-image) to avoid
# buildctl/buildkitd version skew.
# renovate: datasource=docker depName=moby/buildkit
ARG BUILDKIT_VERSION=v0.31.1
# Build on the NATIVE builder arch and cross-compile to $TARGETARCH (pure Go, CGO off): a multi-arch
# build then costs one compile per arch instead of an emulated toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:3aff6657219a4d9c14e27fb1d8976c49c29fddb70ba835014f477e1c70636647 AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/companion ./cmd/companion

# Intentionally tag-pinned (not digest): BUILDKIT_VERSION must track the daemon image
# (buildd --buildkit-image) so buildctl and buildkitd stay in lockstep — a digest here would
# freeze that coupling. No --platform here (unlike the build stage): buildctl ships in the final
# image, so this stage must resolve to the TARGET arch.
FROM moby/buildkit:${BUILDKIT_VERSION} AS buildkit

FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
COPY --from=build /out/companion /companion
COPY --from=buildkit /usr/bin/buildctl /usr/bin/buildctl
USER 65532:65532
ENTRYPOINT ["/companion"]
