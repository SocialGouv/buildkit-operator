# gateway — buildkit-operator's shared SNI router for off-cluster CI. A pure Go binary on distroless; it
# terminates NO TLS (it peeks the ClientHello SNI and pipes the connection), so it carries no certs.
# Build on the NATIVE builder arch and cross-compile to $TARGETARCH (pure Go, CGO off): a multi-arch
# build then costs one compile per arch instead of an emulated toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:32c0e6e5c4f6707717051091b4d0b077464a679eaab563e11474efc5328e2aa5 AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/gateway /gateway
USER 65532:65532
ENTRYPOINT ["/gateway"]
