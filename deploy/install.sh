#!/usr/bin/env bash
set -Eeuo pipefail
if [[ ${EUID} -ne 0 ]]; then echo 'Run with sudo.' >&2; exit 1; fi
bundle_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ ! -f "$bundle_dir/vault" ]]; then echo 'Extract the release bundle before running deploy/install.sh.' >&2; exit 1; fi
if [[ -e /opt/pikpakvalue/vault ]]; then echo 'An installation exists. Use deploy/update.sh to preserve and back up its data.' >&2; exit 1; fi
command -v systemctl >/dev/null
command -v curl >/dev/null
health_url=${VAULT_HEALTH_URL:-http://127.0.0.1:5675/healthz}
id pikpak-vault >/dev/null 2>&1 || useradd --system --home-dir /var/lib/pikpak-vault --shell /usr/sbin/nologin pikpak-vault
install -d -m 0755 /opt/pikpakvalue
install -d -m 0700 -o pikpak-vault -g pikpak-vault /var/lib/pikpak-vault
install -m 0755 "$bundle_dir/vault" /opt/pikpakvalue/vault
install -m 0644 "$bundle_dir/deploy/pikpak-vault.service" /etc/systemd/system/pikpak-vault.service
if [[ ! -e /etc/pikpak-vault.env ]]; then install -m 0600 "$bundle_dir/deploy/pikpak-vault.env.example" /etc/pikpak-vault.env; fi
bash "$bundle_dir/deploy/enable-updates.sh"
systemctl daemon-reload
systemctl enable --now pikpak-vault
for attempt in {1..20}; do
  if systemctl is-active --quiet pikpak-vault && curl --fail --silent --max-time 2 "$health_url"; then
    printf '\nInstalled. Find the initialization code with: sudo journalctl -u pikpak-vault -n 30\n'
    exit 0
  fi
  sleep 1
done
echo 'Service did not become ready. Inspect: journalctl -u pikpak-vault -n 50' >&2
exit 1
