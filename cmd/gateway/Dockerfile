# gateway — buildkit-operator's shared SNI router for off-cluster CI. A pure Go binary on distroless; it
# terminates NO TLS (it peeks the ClientHello SNI and pipes the connection), so it carries no certs.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/gateway /gateway
USER 65532:65532
ENTRYPOINT ["/gateway"]
