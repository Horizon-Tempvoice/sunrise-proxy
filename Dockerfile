# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w \
      -X github.com/Horizon-Tempvoice/sunrise-proxy/internal/buildinfo.Version=${VERSION} \
      -X github.com/Horizon-Tempvoice/sunrise-proxy/internal/buildinfo.Commit=${COMMIT}" \
    -o /out/sunrise ./cmd/sunrise

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/sunrise /sunrise
USER 65532:65532
EXPOSE 8080 9000 7946/tcp 7946/udp
HEALTHCHECK --interval=10s --timeout=5s --start-period=5s CMD ["/sunrise", "healthcheck"]
ENTRYPOINT ["/sunrise"]
