# companion — sidecar. Our Go binary + buildctl (copied from the pinned buildkit
# image) so /readyz and the inode backstop can call `buildctl ... debug workers / prune`.
# Keep BUILDKIT_VERSION aligned with the daemon image (buildd --buildkit-image) to avoid
# buildctl/buildkitd version skew.
# renovate: datasource=docker depName=moby/buildkit
ARG BUILDKIT_VERSION=v0.34.2
# Build on the NATIVE builder arch and cross-compile to $TARGETARCH (pure Go, CGO off): a multi-arch
# build then costs one compile per arch instead of an emulated toolchain.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS build
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

FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/companion /companion
COPY --from=buildkit /usr/bin/buildctl /usr/bin/buildctl
USER 65532:65532
ENTRYPOINT ["/companion"]
