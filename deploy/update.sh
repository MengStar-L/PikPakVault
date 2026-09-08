#!/usr/bin/env bash
# Submit the same durable update request used by the authenticated web UI.
set -Eeuo pipefail
[[ $EUID -eq 0 ]] || { echo 'Run with sudo.' >&2; exit 1; }
tag=${1:-}
[[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo 'Usage: sudo bash deploy/update.sh vX.Y.Z' >&2; exit 1; }
command -v systemctl >/dev/null
[[ -x /usr/local/lib/pikpak-vault/maintenance ]] || { echo 'Install the update components first.' >&2; exit 1; }
systemctl is-active --quiet pikpak-vault-update.service && { echo 'An updater is running.' >&2; exit 1; }
request_file=/var/lib/pikpak-vault/update-request.json
token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
request_tmp=$(mktemp /var/lib/pikpak-vault/.update-request-XXXXXX)
trap 'rm -f -- "$request_tmp"' EXIT
printf '{"tag":"%s","token":"%s"}\n' "$tag" "$token" > "$request_tmp"
chown pikpak-vault:pikpak-vault "$request_tmp"
chmod 0600 "$request_tmp"
ln -- "$request_tmp" "$request_file"
systemctl start --no-block pikpak-vault-update.service
echo 'Update requested. Follow: sudo journalctl -fu pikpak-vault-update'
echo 'Progress: sudo cat /var/lib/pikpak-vault-updater/status.json'
