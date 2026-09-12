#!/usr/bin/env bash
# Install the plugin deploy webhook on cn2. Run as root.
#
#   ssh cn2 'bash -s' < deploy/setup-cn2.sh
#
# Idempotent: safe to re-run. It never overwrites an existing env file, so the
# WEBHOOK_SECRET you set the first time survives re-runs.
set -euo pipefail

INSTALL_DIR=/opt/plugin-deploy
ENV_FILE="$INSTALL_DIR/plugin-deploy.env"
REPO_DIR="${REPO_DIR:-$PWD}"

echo "==> creating $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"

echo "==> installing deploy-webhook.py"
install -m 0755 "$REPO_DIR/deploy/deploy-webhook.py" "$INSTALL_DIR/deploy-webhook.py"

if [ ! -f "$ENV_FILE" ]; then
  echo "==> creating $ENV_FILE"
  if [ -f "$REPO_DIR/deploy/plugin-deploy.env.example" ]; then
    install -m 0600 "$REPO_DIR/deploy/plugin-deploy.env.example" "$ENV_FILE"
    # Generate a secret so the listener refuses to start without one, and so
    # there is a concrete value to paste into the GitHub webhook / CI secret.
    secret=$(python3 -c 'import secrets;print(secrets.token_hex(32))')
    sed -i.bak "s|^WEBHOOK_SECRET=.*|WEBHOOK_SECRET=$secret|" "$ENV_FILE"
    rm -f "$ENV_FILE.bak"
    echo "    generated WEBHOOK_SECRET"
  else
    echo "ERROR: deploy/plugin-deploy.env.example not found in $REPO_DIR" >&2
    exit 1
  fi
else
  echo "==> $ENV_FILE already exists; leaving it untouched"
fi

chmod 600 "$ENV_FILE"

echo "==> installing systemd unit"
python3 "$INSTALL_DIR/deploy-webhook.py" install

echo "==> reloading systemd"
systemctl daemon-reload
systemctl enable --now plugin-deploy-webhook.service

sleep 1
echo "==> status"
systemctl is-active plugin-deploy-webhook.service || true
systemctl --no-pager -l status plugin-deploy-webhook.service | head -20 || true

echo
echo "==> local health check"
curl -fsS --max-time 5 http://127.0.0.1:$(grep -E '^LISTEN_PORT=' "$ENV_FILE" | cut -d= -f2)/health || {
  echo "health check failed; logs:"; journalctl -u plugin-deploy-webhook -n 30 --no-pager || true; }

echo
echo "-------------------------------------------------------------------"
echo "Webhook secret (put this in the GitHub repo secret DEPLOY_WEBHOOK_SECRET):"
grep -E '^WEBHOOK_SECRET=' "$ENV_FILE" | cut -d= -f2-
echo
echo "Public URL: https://cliproxy.onlylike.work/hooks/plugin-deploy"
echo "(add the Caddy snippet below to /etc/caddy/Caddyfile, then: systemctl reload caddy)"
echo "-------------------------------------------------------------------"
cat <<'SNIPPET'

handle /hooks/plugin-deploy* {
    reverse_proxy 127.0.0.1:9000
}

SNIPPET
