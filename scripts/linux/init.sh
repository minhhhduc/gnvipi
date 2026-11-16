#!/usr/bin/env bash
# One-time setup: create an empty internal/models/playground_models.json.
# Usage: ./scripts/linux/init.sh [--force]
set -euo pipefail

FORCE=0
for arg in "$@"; do
  case "$arg" in
    -f|--force) FORCE=1 ;;
    -h|--help)
      echo "Usage: $0 [--force]"
      exit 0
      ;;
    *)
      echo "Unknown option: $arg" >&2
      exit 2
      ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
TARGET="$ROOT_DIR/internal/models/playground_models.json"

if [[ -f "$TARGET" && "$FORCE" -eq 0 ]]; then
  echo "already there, skipping: $TARGET"
  exit 0
fi

mkdir -p "$(dirname "$TARGET")"
cat > "$TARGET" <<'EOF'
{
  "groups": [],
  "masks_id": [],
  "deleted_id": [],
  "schema_version": 2,
  "providers_v2": []
}
EOF
echo "created $TARGET (empty catalog - run Rescan on the Models page)"
