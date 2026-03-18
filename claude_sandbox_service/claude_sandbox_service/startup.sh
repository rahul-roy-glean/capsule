#!/bin/bash
set -euo pipefail

echo "[startup] Launching Claude sandbox service..."

# Start GCE metadata emulator for cross-cloud Vertex AI authentication (AWS only)
if [ "${START_GCE_METADATA_EMULATOR:-}" = "1" ]; then
    echo "[startup] Starting GCE metadata emulator for Vertex AI..."
    python3 /opt/gce-metadata-emulator/gce_metadata_emulator.py &
    sleep 1
    echo "[startup] GCE metadata emulator started"
fi

sudo /usr/sbin/sshd -D &

if [ -f "/etc/mcp-config/mcp-config.json" ]; then
    cp /etc/mcp-config/mcp-config.json /home/claude/.claude.json
    chown 1000:1000 /home/claude/.claude.json
    echo "[startup] MCP configuration applied successfully"
fi

if [ -n "${HTTPS_PROXY:-}" ]; then
    git config --global http.proxy "$HTTPS_PROXY"
    git config --global https.proxy "$HTTPS_PROXY"
    git config --global http.proxyAuthMethod basic
fi

# Snapshot remote branches at startup for branch naming enforcement
# This allows the Stop hook to distinguish between pre-existing branches and newly created ones
REPO_DIR="${REPO_DIR:-/workspace/repo}"
SNAPSHOT_FILE="/opt/claude-hooks/remote-branches-snapshot"
if [ -d "$REPO_DIR/.git" ]; then
    if git -C "$REPO_DIR" branch -r > "$SNAPSHOT_FILE" 2>/dev/null; then
        echo "[startup] Remote branches snapshot created"
    else
        echo "[startup] Warning: Failed to create remote branches snapshot"
    fi
fi

# Install Glean Code Writer commit branding hook
if [ -f /home/claude/.git-helpers/install-branding-hook.sh ] && [ -d /workspace/repo/.git ]; then
    if /home/claude/.git-helpers/install-branding-hook.sh /workspace/repo; then
        echo "[startup] Installed commit branding hook"
    else
        echo "[startup] Warning: failed to install branding hook"
    fi
fi

cd /app
exec gunicorn \
  --config /app/gunicorn_config.py \
  python_scio.platform.common.app.claude_sandbox_service.claude_sandbox_service:app
