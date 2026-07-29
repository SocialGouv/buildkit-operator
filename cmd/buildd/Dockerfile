# buildd — control plane (controller-runtime manager + /route API). Distroless, nonroot.
# Build on the NATIVE builder arch and cross-compile to $TARGETARCH (pure Go, CGO off): a multi-arch
# build then costs one compile per arch instead of an emulated toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:32c0e6e5c4f6707717051091b4d0b077464a679eaab563e11474efc5328e2aa5 AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/buildd ./cmd/buildd

FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
COPY --from=build /out/buildd /buildd
USER 65532:65532
ENTRYPOINT ["/buildd"]
