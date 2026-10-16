# syntax=docker/dockerfile:1

# --- Сборка: полный Go-тулчейн, в итоговый образ не попадает ---
FROM golang:1.27-alpine AS build
WORKDIR /src

# Сначала только зависимости: слой кэшируется, пока не меняется go.sum
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 — статический бинарник без libc, запускается в пустом образе.
# -trimpath убирает локальные пути из бинарника, -ldflags "-s -w" — отладочную информацию.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# --- Итоговый образ: только бинарники, CA-сертификаты и пользователь nonroot ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /out/worker /app/
USER nonroot:nonroot
EXPOSE 8000
ENTRYPOINT ["/app/api"]
