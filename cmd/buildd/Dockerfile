# buildd — control plane (controller-runtime manager + /route API). Distroless, nonroot.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/buildd ./cmd/buildd

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/buildd /buildd
USER 65532:65532
ENTRYPOINT ["/buildd"]
