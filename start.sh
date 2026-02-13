#!/bin/bash
# Ensure config files are writable for WebUI settings
set -euo pipefail

for f in config.yaml nodes.txt; do
  if [ -d "$f" ]; then
    echo "⚠️  $f is a directory; removing and recreating as a file..."
    rm -rf "$f"
  fi
  touch "$f" 2>/dev/null || true
done

chmod 666 config.yaml nodes.txt 2>/dev/null || true
docker compose down
docker compose build --pull
docker compose up -d
