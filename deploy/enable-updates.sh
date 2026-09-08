#!/usr/bin/env bash
set -Eeuo pipefail
[[ $EUID -eq 0 ]] || { echo 'Run with sudo.' >&2; exit 1; }
bundle_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
[[ -f "$bundle_dir/vault" && -f /opt/pikpakvalue/vault ]] || { echo 'Install PikPak Vault first.' >&2; exit 1; }
install -d -m 0755 /usr/local/lib/pikpak-vault /var/lib/pikpak-vault-updater
install -m 0755 "$bundle_dir/vault" /usr/local/lib/pikpak-vault/maintenance
install -m 0644 "$bundle_dir/deploy/pikpak-vault-update.service" /etc/systemd/system/
install -m 0644 "$bundle_dir/deploy/pikpak-vault-update.path" /etc/systemd/system/
systemctl daemon-reload
systemctl enable pikpak-vault-update.service
systemctl enable --now pikpak-vault-update.path
systemctl start pikpak-vault-update.service
