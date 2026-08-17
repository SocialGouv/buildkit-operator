# buildd — control plane (controller-runtime manager + /route API). Distroless, nonroot.
# Build on the NATIVE builder arch and cross-compile to $TARGETARCH (pure Go, CGO off): a multi-arch
# build then costs one compile per arch instead of an emulated toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6 AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/buildd ./cmd/buildd

FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
COPY --from=build /out/buildd /buildd
USER 65532:65532
ENTRYPOINT ["/buildd"]
