#!/usr/bin/env bash
set -Eeuo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
version=${1:-0.2.1}
npm ci --prefix web
npm run build --prefix web
go test ./...
mkdir -p bin
for arch in amd64 arm64; do
  bundle="bin/pikpak-vault-$version-linux-$arch"
  mkdir -p "$bundle"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags="-s -w -X pikpakvault/internal/vault.Version=$version" -o "$bundle/vault" ./cmd/vault
  cp -R deploy "$bundle/"
  cp -R docs "$bundle/"
  cp README.md THIRD_PARTY.md "$bundle/"
  go run ./scripts/package "$bundle" "bin/pikpak-vault-$version-linux-$arch.tar.gz"
done
(cd bin && sha256sum "pikpak-vault-$version-linux-amd64.tar.gz" "pikpak-vault-$version-linux-arm64.tar.gz" > SHA256SUMS)
