FROM node:24-alpine AS frontend
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.25-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/web/dist ./web/dist
ARG TARGETARCH
ARG VERSION=0.3.3
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w -X pikpakvault/internal/vault.Version=${VERSION}" -o /vault ./cmd/vault

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata && addgroup -S -g 10001 vault && adduser -S -u 10001 -G vault vault && mkdir -p /data /opt/pikpakvalue/runtime /opt/pikpakvalue/downloads && chown vault:vault /data /opt/pikpakvalue/runtime /opt/pikpakvalue/downloads
COPY --from=backend /vault /opt/pikpakvalue/vault
USER vault
ENV VAULT_DATA=/data VAULT_LISTEN=0.0.0.0:5675
VOLUME ["/data", "/opt/pikpakvalue/runtime", "/opt/pikpakvalue/downloads"]
EXPOSE 5675
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s CMD wget -q -O /dev/null http://127.0.0.1:5675/healthz || exit 1
ENTRYPOINT ["/opt/pikpakvalue/vault"]
