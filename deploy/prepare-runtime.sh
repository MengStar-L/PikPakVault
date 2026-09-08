#!/usr/bin/env bash
# Grant only the managed dependency and download directories to the service.
set -Eeuo pipefail
[[ $EUID -eq 0 ]] || { echo 'Run with sudo.' >&2; exit 1; }
install -d -m 0700 -o pikpak-vault -g pikpak-vault /opt/pikpakvalue/runtime /opt/pikpakvalue/downloads
install -d -m 0755 /etc/systemd/system/pikpak-vault.service.d
cat > /etc/systemd/system/pikpak-vault.service.d/runtime.conf <<'EOF'
[Service]
ReadWritePaths=/opt/pikpakvalue/runtime /opt/pikpakvalue/downloads
EOF
systemctl daemon-reload
