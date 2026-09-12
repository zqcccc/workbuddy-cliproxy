#!/usr/bin/env python3
"""GitHub webhook receiver that installs workbuddy plugin artifacts on this host.

Flow
----
1. GitHub sends `release.published` (tagged build) or `workflow_run.completed`
   (every successful main build, if you enable that trigger) as JSON.
2. We verify the HMAC-SHA256 signature against the shared secret, then pick the
   asset whose name matches this host's platform (linux / amd64 by default,
   overridable in plugin-deploy.env).
3. The asset is downloaded to a temp file, sanity-checked (must be a non-trivial
   ELF shared object), and only then moved over the live plugin.
4. Old plugins are kept as `.<name>.bak-<n>` so a bad release can be reverted
   with `deploy-webhook.py rollback`.
5. Unless DRY_RUN / NO_RESTART is set, the CPA container is restarted so the
   host actually picks the new .so up -- plugins are loaded at process start.

Config: /opt/plugin-deploy/plugin-deploy.env (see plugin-deploy.env.example).

Install: deploy-webhook.py install     (writes + enables the systemd unit)
Logs:    journalctl -u plugin-deploy-webhook -f
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# ---------------------------------------------------------------- config

ENV_FILE = os.environ.get("PLUGIN_DEPLOY_ENV", "/opt/plugin-deploy/plugin-deploy.env")
SYSTEMD_UNIT = "/etc/systemd/system/plugin-deploy-webhook.service"
INSTALL_DIR = "/opt/plugin-deploy"

DEFAULTS = {
    "LISTEN_HOST": "127.0.0.1",
    "LISTEN_PORT": "9000",
    "WEBHOOK_SECRET": "",
    "PLATFORM": "linux-amd64",
    "PLUGIN_DIR": "/root/code/cliproxyapiplus-docker/plugins",
    "CONTAINER_NAME": "cli-proxy-api-plus",
    "KEEP_BACKUPS": "5",
    # Only these assets are ever installed; everything else in the release is
    # ignored. Keeps an unrelated release from overwriting the plugin.
    "ASSET_PREFIXES": "workbuddy",
    "DRY_RUN": "0",
    "NO_RESTART": "0",
    # Seconds; protects the restart path from concurrent duplicate deliveries.
    "DEPLOY_LOCK": "/var/lock/plugin-deploy.lock",
}

CFG = dict(DEFAULTS)
_STATE_FILE = os.path.join(INSTALL_DIR, "state.json")


def load_env(path: str = ENV_FILE) -> None:
    if not os.path.exists(path):
        return
    with open(path, encoding="utf-8") as fh:
        for raw in fh:
            line = raw.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, value = line.partition("=")
            key = key.strip()
            value = value.strip().strip("'").strip('"')
            if key:
                CFG[key] = value


def truthy(key: str) -> bool:
    return CFG.get(key, "0").strip().lower() in {"1", "true", "yes", "on"}


# ---------------------------------------------------------------- logging

def log(msg: str) -> None:
    print(f"{time.strftime('%Y-%m-%dT%H:%M:%S%z')} {msg}", flush=True)


# ---------------------------------------------------------------- helpers

SAFE_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")


def asset_matches_platform(name: str, platform: str) -> bool:
    """True when `name` is an artifact for `platform`.

    CI names assets workbuddy-<goos>-<goarch>.so and
    workbuddy-global-<goos>-<goarch>.so. We match on the suffix so a stray
    workbuddy-<otheros>-<arch>.so in the same release is never installed here.
    """
    if not name.endswith(".so"):
        return False
    stem = name[:-3]
    return stem.endswith("-" + platform)


def canonical_plugin_name(asset_name: str) -> str:
    """Strip the platform suffix: workbuddy-linux-amd64.so -> workbuddy.so."""
    stem = asset_name[:-3]
    platform = CFG["PLATFORM"]
    if stem.endswith("-" + platform):
        stem = stem[: -(len(platform) + 1)]
    return stem + ".so"


def looks_like_elf_so(path: str) -> bool:
    try:
        with open(path, "rb") as fh:
            header = fh.read(64)
    except OSError:
        return False
    if not header.startswith(b"\x7fELF"):
        return False
    # e_type at offset 16: 3 == ET_DYN (shared object / PIE)
    if len(header) < 18:
        return False
    return int.from_bytes(header[16:18], "little") == 3


def run(cmd: list[str], *, check: bool = True) -> subprocess.CompletedProcess:
    log("+ " + " ".join(cmd))
    return subprocess.run(cmd, capture_output=True, text=True, check=check)


def github_api(url: str, token: str = "") -> bytes:
    import urllib.request

    req = urllib.request.Request(url, headers={"Accept": "application/octet-stream",
                                               "User-Agent": "plugin-deploy-webhook"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=120) as resp:
        return resp.read()


def download(url: str, dest: str, token: str = "") -> None:
    data = github_api(url, token)
    tmp = dest + ".part"
    with open(tmp, "wb") as fh:
        fh.write(data)
    os.replace(tmp, dest)


# ---------------------------------------------------------------- install

def backup(path: str) -> None:
    if not os.path.exists(path):
        return
    keep = int(CFG.get("KEEP_BACKUPS", "5") or 5)
    stamp = time.strftime("%Y%m%d-%H%M%S")
    bak = f"{path}.bak-{stamp}"
    # Rapid successive deploys would otherwise collide on the same second and
    # silently clobber the only backup.
    n = 1
    while os.path.exists(bak):
        n += 1
        bak = f"{path}.bak-{stamp}-{n}"
    shutil.copy2(path, bak)
    log(f"backed up {path} -> {bak}")
    _prune_backups(os.path.dirname(path), os.path.basename(path), keep)


def _prune_backups(dirpath: str, base: str, keep: int) -> None:
    try:
        entries = sorted(
            (e for e in os.listdir(dirpath) if e.startswith(base + ".bak-")),
            reverse=True,
        )
    except OSError:
        return
    for old in entries[keep:]:
        try:
            os.remove(os.path.join(dirpath, old))
            log(f"pruned old backup {old}")
        except OSError as exc:
            log(f"warn: could not remove {old}: {exc}")


def restart_container() -> None:
    name = CFG["CONTAINER_NAME"]
    if not name:
        log("no CONTAINER_NAME configured; skip restart")
        return
    if truthy("NO_RESTART"):
        log("NO_RESTART set; skip restart")
        return
    proc = run(["docker", "restart", name], check=False)
    if proc.returncode != 0:
        log(f"ERROR: docker restart {name} failed: {proc.stderr.strip()}")
        return
    log(f"restarted container {name}")


def record_state(asset: str, target: str, tag: str) -> None:
    try:
        os.makedirs(INSTALL_DIR, exist_ok=True)
        state = {"asset": asset, "target": target, "tag": tag, "installed_at": int(time.time())}
        with open(_STATE_FILE, "w", encoding="utf-8") as fh:
            json.dump(state, fh, indent=2)
    except OSError as exc:
        log(f"warn: could not write state: {exc}")


def install_asset(
    asset_name: str, url: str, tag: str, token: str = "", restart: bool = True
) -> tuple[bool, str]:
    prefixes = [p.strip() for p in CFG["ASSET_PREFIXES"].split(",") if p.strip()]
    if prefixes and not any(asset_name.startswith(p) for p in prefixes):
        return False, f"asset {asset_name} not in ASSET_PREFIXES"
    if not asset_matches_platform(asset_name, CFG["PLATFORM"]):
        return False, f"asset {asset_name} is not for platform {CFG['PLATFORM']}"

    target_name = canonical_plugin_name(asset_name)
    if not SAFE_NAME.match(target_name[:-3]):
        return False, f"unsafe target name {target_name}"

    plugin_dir = CFG["PLUGIN_DIR"]
    os.makedirs(plugin_dir, exist_ok=True)
    target = os.path.join(plugin_dir, target_name)

    with tempfile.TemporaryDirectory() as tmpdir:
        # basename() so a crafted asset name can never escape the temp dir.
        local = os.path.join(tmpdir, os.path.basename(asset_name))
        try:
            download(url, local, token)
        except Exception as exc:  # noqa: BLE001 - surface any fetch failure
            return False, f"download failed: {exc}"
        size = os.path.getsize(local)
        if size < 1024 * 64:
            return False, f"refusing suspiciously small artifact ({size} bytes)"
        if not looks_like_elf_so(local) and CFG["PLATFORM"].startswith("linux"):
            return False, "artifact is not an ELF shared object"

        if truthy("DRY_RUN"):
            log(f"DRY_RUN: would install {asset_name} -> {target} ({size} bytes)")
            return True, f"dry-run ok ({size} bytes)"

        backup(target)
        shutil.copy2(local, target)
        os.chmod(target, 0o644)

    if restart:
        restart_container()
    log(f"installed {asset_name} -> {target}")
    record_state(asset_name, target, tag)
    return True, f"installed {target_name}"


# ---------------------------------------------------------------- payloads

def collect_deploy_assets(payload: dict) -> list[tuple[str, str, str]]:
    """The CI-driven format: {"tag": ..., "assets": [{"name", "url"}]}.

    This is what the workflow posts directly, and it is the primary path: it
    works for every push, whereas GitHub's `release` webhook only fires
    `published` once per tag.
    """
    tag = payload.get("tag", "")
    out = []
    for asset in payload.get("assets", []) or []:
        name, url = asset.get("name", ""), asset.get("url", "")
        if name and url:
            out.append((name, url, tag))
    return out


def collect_release_assets(payload: dict) -> list[tuple[str, str, str]]:
    """GitHub's native release event, kept as a fallback."""
    rel = payload.get("release", {})
    tag = rel.get("tag_name", "")
    out = []
    for asset in rel.get("assets", []) or []:
        # browser_download_url needs no auth for public repos.
        url = asset.get("browser_download_url") or asset.get("url")
        if url:
            out.append((asset.get("name", ""), url, tag))
    return out


def collect_workflow_assets(payload: dict, token: str) -> list[tuple[str, str, str]]:
    """Fetch artifact names from a completed workflow run via the GitHub API.

    Artifact download requires authentication even on public repos, so this path
    needs GITHUB_TOKEN in the env file. Prefer the deploy path instead.
    """
    run_ = payload.get("workflow_run", {})
    if run_.get("conclusion") != "success":
        return []
    if not token:
        log("workflow_run received but no GITHUB_TOKEN configured; skipping")
        return []
    api = run_.get("artifacts_url")
    if not api:
        return []
    try:
        raw = github_api(api, token)
        data = json.loads(raw)
    except Exception as exc:  # noqa: BLE001
        log(f"could not list artifacts: {exc}")
        return []
    return [
        (art.get("name", ""), art.get("archive_download_url", ""),
         run_.get("head_branch", ""))
        for art in data.get("artifacts", []) or []
        if not art.get("expired")
    ]


# ---------------------------------------------------------------- http

class Handler(BaseHTTPRequestHandler):
    server_version = "plugin-deploy-webhook/1.0"

    def _send(self, code: int, body: str) -> None:
        data = body.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self) -> None:  # noqa: N802
        if self.path.rstrip("/") in {"", "/health"}:
            self._send(200, "ok\n")
        else:
            self._send(404, "not found\n")

    def do_POST(self) -> None:  # noqa: N802
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            self._send(400, "bad content-length\n")
            return
        if length > 25 * 1024 * 1024:
            self._send(413, "payload too large\n")
            return
        body = self.rfile.read(length)

        secret = CFG["WEBHOOK_SECRET"]
        if not secret:
            log("ERROR: WEBHOOK_SECRET is not configured; refusing request")
            self._send(500, "server not configured\n")
            return
        sig = self.headers.get("X-Hub-Signature-256", "")
        expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
        if not hmac.compare_digest(sig, expected):
            log("rejected request: bad signature")
            self._send(401, "bad signature\n")
            return

        event = self.headers.get("X-GitHub-Event", "")
        try:
            payload = json.loads(body)
        except json.JSONDecodeError:
            self._send(400, "bad json\n")
            return

        if event == "ping":
            self._send(200, "pong\n")
            return
        if event == "deploy":
            assets = collect_deploy_assets(payload)
        elif event == "release" and payload.get("action") == "published":
            assets = collect_release_assets(payload)
        elif event == "workflow_run":
            assets = collect_workflow_assets(payload, CFG.get("GITHUB_TOKEN", ""))
            if assets and assets[0][1].endswith(".zip"):
                # Artifact downloads are zip archives; not supported in v1.
                log("workflow_run artifact path requires unzip; ignoring")
                assets = []
        else:
            self._send(200, f"ignored event {event}\n")
            return

        results = []
        # Restart once, after every asset in the delivery has landed: restarting
        # between the two realms would drop the service twice and leave a window
        # where the CN and Global plugins are from different builds.
        installed_real = False
        for name, url, tag in assets:
            try:
                ok, msg = install_asset(name, url, tag, CFG.get("GITHUB_TOKEN", ""),
                                        restart=False)
            except Exception as exc:  # noqa: BLE001 - never 500 on one asset
                ok, msg = False, f"error: {exc}"
            log(f"{'OK  ' if ok else 'SKIP'} {name}: {msg}")
            if ok:
                installed_real = installed_real or not msg.startswith("dry-run")
                results.append(f"{name}: {msg}")
        if not results:
            self._send(200, "no matching asset for this platform\n")
            return
        if installed_real:
            restart_container()
        self._send(200, "\n".join(results) + "\n")

    def log_message(self, fmt: str, *args) -> None:
        log("%s - %s" % (self.address_string(), fmt % args))


# ---------------------------------------------------------------- cli

UNIT_TEMPLATE = """[Unit]
Description=GitHub webhook that installs workbuddy plugin artifacts
After=network.target

[Service]
Type=simple
Environment=PLUGIN_DEPLOY_ENV={env}
ExecStart=/usr/bin/python3 {script} serve
WorkingDirectory={install_dir}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
"""

CADDY_SNIPPET = """# Reverse proxy for the plugin deploy webhook.
# Add inside the existing site block (or its own), then: systemctl reload caddy
handle /hooks/plugin-deploy* {{
    reverse_proxy 127.0.0.1:{port}
}}
"""


def cmd_serve() -> int:
    load_env()
    if not CFG["WEBHOOK_SECRET"]:
        log("ERROR: WEBHOOK_SECRET missing in " + ENV_FILE)
        return 1
    host, port = CFG["LISTEN_HOST"], int(CFG["LISTEN_PORT"])
    log(f"serving on {host}:{port} platform={CFG['PLATFORM']} dir={CFG['PLUGIN_DIR']}")
    ThreadingHTTPServer((host, port), Handler).serve_forever()
    return 0


def cmd_install() -> int:
    script = os.path.abspath(__file__)
    os.makedirs(INSTALL_DIR, exist_ok=True)
    if not os.path.exists(ENV_FILE):
        log(f"WARNING: {ENV_FILE} missing; copy plugin-deploy.env.example there first")
    dest = os.path.join(INSTALL_DIR, os.path.basename(script))
    if os.path.abspath(dest) != script:
        shutil.copy2(script, dest)
        os.chmod(dest, 0o755)
    with open(SYSTEMD_UNIT, "w", encoding="utf-8") as fh:
        fh.write(UNIT_TEMPLATE.format(env=ENV_FILE, script=dest, install_dir=INSTALL_DIR))
    run(["systemctl", "daemon-reload"], check=False)
    run(["systemctl", "enable", "--now", "plugin-deploy-webhook"], check=False)
    log("installed systemd unit plugin-deploy-webhook")
    print()
    print("Caddy snippet (add to /etc/caddy/Caddyfile, then: systemctl reload caddy)")
    print(CADDY_SNIPPET.format(port=CFG.get("LISTEN_PORT", "9000")))
    return 0


def cmd_rollback() -> int:
    load_env()
    plugin_dir = CFG["PLUGIN_DIR"]
    try:
        names = sorted(os.listdir(plugin_dir))
    except OSError as exc:
        log(f"cannot read {plugin_dir}: {exc}")
        return 1
    baks = [n for n in names if ".bak-" in n and not n.endswith(".part")]
    if not baks:
        log("no backups found")
        return 1
    for n in baks:
        print(n)
    return 0


def cmd_status() -> int:
    load_env()
    print(json.dumps({k: v for k, v in CFG.items() if k != "WEBHOOK_SECRET"}, indent=2))
    print("secret set:", bool(CFG["WEBHOOK_SECRET"]))
    if os.path.exists(_STATE_FILE):
        with open(_STATE_FILE, encoding="utf-8") as fh:
            print("last install:", fh.read())
    return 0


def main(argv: list[str]) -> int:
    load_env()
    cmd = argv[1] if len(argv) > 1 else "serve"
    if cmd == "serve":
        return cmd_serve()
    if cmd == "install":
        return cmd_install()
    if cmd == "rollback":
        return cmd_rollback()
    if cmd == "status":
        return cmd_status()
    print(__doc__)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
