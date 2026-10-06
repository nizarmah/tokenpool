FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/tokenpool ./cmd/tokenpool

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/nizarmah/tokenpool" \
      org.opencontainers.image.description="Failover proxy that pools LLM API keys" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/tokenpool /tokenpool
EXPOSE 8080
ENTRYPOINT ["/tokenpool"]
CMD ["-config", "/etc/tokenpool/tokenpool.yaml", "-json-logs"]
