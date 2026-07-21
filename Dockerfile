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
# The out-of-band reset/init tool (run explicitly, e.g. `docker compose run --rm --entrypoint /app/reset combas-server -confirm`).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/reset ./cmd/reset
# capture tool
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/capture ./cmd/capture
# sim tool
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/simulate ./cmd/simulate
# seed history
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/seedhistory ./cmd/seedhistory
# reconcile squads
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/reconcile-squads ./cmd/reconcile-squads
# gamertag backfill (corrects host-clobbered gamertags from the webservices players collection)
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/backfill-gamertags ./cmd/backfill-gamertags
# unidentified weapon trigger util
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/unidentified-weapon ./cmd/unidentified-weapon

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=build /out/combas-server /app/combas-server
COPY --from=build /out/reset /app/reset
COPY --from=build /out/capture /app/capture
COPY --from=build /out/simulate /app/simulate
COPY --from=build /out/seedhistory /app/seedhistory
COPY --from=build /out/reconcile-squads /app/reconcile-squads
COPY --from=build /out/backfill-gamertags /app/backfill-gamertags
COPY --from=build /out/unidentified-weapon /app/unidentified-weapon
COPY --from=build /src/cmd/simulate/scenarios /app/
# Baked default config; MONGO_URI / MONGO_DATABASE / LISTENING_ADDRESS env vars override at runtime.
COPY config.toml /app/config.toml

# UDP game-service ports (see config.toml) and the Prometheus metrics endpoint. EXPOSE is documentation;
# docker-compose performs the actual host publishing.
EXPOSE 1201-1258/udp
EXPOSE 9090

ENTRYPOINT ["/app/combas-server"]
