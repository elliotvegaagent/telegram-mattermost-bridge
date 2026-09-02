FROM golang:1.24-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/bridge ./cmd/bridge

FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install --yes --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 999 bridge \
    && useradd --system --uid 999 --gid 999 --home-dir /app bridge \
    && mkdir -p /app /data \
    && chown -R bridge:bridge /app /data

COPY --from=build /out/bridge /app/bridge
USER bridge
WORKDIR /app
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/app/bridge"]
CMD ["run"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/app/bridge", "healthcheck"]
