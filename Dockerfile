# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.24 AS build
WORKDIR /src

# Cache module downloads separately from source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go build (the Mongo driver needs no cgo) so we can run on a static distroless image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/combas-server .

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=build /out/combas-server /app/combas-server
# Baked default config; MONGO_URI / MONGO_DATABASE / LISTENING_ADDRESS env vars override at runtime.
COPY config.toml /app/config.toml

# UDP game-service ports (see config.toml) and the Prometheus metrics endpoint. EXPOSE is documentation;
# docker-compose performs the actual host publishing.
EXPOSE 1201-1258/udp
EXPOSE 9090

ENTRYPOINT ["/app/combas-server"]
