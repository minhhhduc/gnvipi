#!/usr/bin/env bash
# Deploy gnvipi on macOS via Docker Compose (db + migrate + serve + optional tunnel).
set -euo pipefail

BUILD=1
DOWN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-build) BUILD=0; shift ;;
    --down)     DOWN=1; shift ;;
    -h|--help)
      cat <<'EOF'
Usage: ./scripts/macos/deploy.sh [options]
  --no-build  Start containers without rebuilding images
  --down      Stop and remove running Compose containers (keeps volumes)
EOF
      exit 0
      ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$ROOT"

if ! command -v docker >/dev/null 2>&1; then
  echo "Error: docker is not installed or not in PATH." >&2
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "Error: Docker Desktop is not running." >&2
  exit 1
fi

ENV_ARGS=()
if [[ -f ".env" ]]; then
  ENV_ARGS=(--env-file ".env")
else
  echo "Warning: .env not found in $ROOT; using environment defaults."
fi

if [[ "$DOWN" -eq 1 ]]; then
  docker compose "${ENV_ARGS[@]}" down
  exit 0
fi

UP_ARGS=(up -d)
if [[ "$BUILD" -eq 1 ]]; then
  UP_ARGS+=(--build)
fi

docker compose "${ENV_ARGS[@]}" "${UP_ARGS[@]}"

PORT="${GNVIPI_PORT:-8080}"
echo "Deployed. Local Admin: http://127.0.0.1:${PORT}/admin"
