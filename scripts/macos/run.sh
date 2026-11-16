#!/usr/bin/env bash
# Start the NVIDIA playground gateway on macOS (with optional Claude Code launch).
set -euo pipefail

PORT=8080
PROXY=""
POOL_SIZE=6
POOL_WORKERS=3
BUILD=0
LAN=0
NO_CAPTCHA=0
CLAUDE=0
HARNESS=0
BATCH=6
CHAMPION_BUDGET=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --port) PORT="${2:?--port requires a value}"; shift 2 ;;
    --proxy) PROXY="${2:?--proxy requires a value}"; shift 2 ;;
    --pool-size) POOL_SIZE="${2:?--pool-size requires a value}"; shift 2 ;;
    --pool-workers) POOL_WORKERS="${2:?--pool-workers requires a value}"; shift 2 ;;
    --build) BUILD=1; shift ;;
    --lan) LAN=1; shift ;;
    --no-captcha) NO_CAPTCHA=1; shift ;;
    --claude) CLAUDE=1; shift ;;
    --harness) HARNESS=1; shift ;;
    --batch) BATCH="${2:?--batch requires a value}"; shift 2 ;;
    --champion-budget) CHAMPION_BUDGET="${2:?--champion-budget requires a value}"; shift 2 ;;
    -h|--help)
      cat <<'EOF'
Usage: ./scripts/macos/run.sh [options]
  --port <port>             Listen port (default: 8080)
  --lan                     Bind to :PORT instead of 127.0.0.1:PORT
  --no-captcha              Disable Chrome captcha pool (custom providers only)
  --build                   Build ./bin/serve before starting
  --claude                  Wait for /healthz and launch Claude Code against the gateway
  --proxy <url>             Proxy URL for Chrome captcha & upstream
  --pool-size <n>           Captcha pool size (default: 6)
  --pool-workers <n>        Captcha pool workers (default: 3)
  --harness                 Enable captcha harness mode
  --batch <n>               Captcha batch size (default: 6)
  --champion-budget <dur>   Benchmark fastest playground URL
EOF
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      exit 2
      ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$ROOT_DIR"

if [[ ! -f "$ROOT_DIR/internal/models/playground_models.json" ]]; then
  "$SCRIPT_DIR/init.sh"
fi

if ! command -v go >/dev/null 2>&1; then
  echo "go not found in PATH" >&2
  exit 1
fi

if [[ -f "$ROOT_DIR/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$ROOT_DIR/.env"
  set +a
fi

DB_URL="${GNVIPI_DATABASE_URL:-postgres://gnvipi_dev:gnvipi_dev_only@localhost:5432/gnvipi?sslmode=disable}"
export GNVIPI_DATABASE_URL="$DB_URL"

if command -v pg_isready >/dev/null 2>&1; then
  if ! pg_isready -d "$DB_URL" >/dev/null 2>&1; then
    if command -v docker >/dev/null 2>&1; then
      docker compose up -d --wait db
    else
      echo "PostgreSQL is not ready. Start it with: docker compose up -d db" >&2
      exit 1
    fi
  fi
fi

EXE="$ROOT_DIR/bin/serve"
if [[ "$BUILD" -eq 1 || ! -x "$EXE" ]]; then
  mkdir -p "$ROOT_DIR/bin"
  echo "building ./bin/serve..."
  go build -o "$EXE" ./cmd/serve
fi

"$EXE" migrate

if [[ "$LAN" -eq 1 ]]; then
  ADDR=":$PORT"
else
  ADDR="127.0.0.1:$PORT"
fi

ARGS=(-auto -addr "$ADDR")
if [[ "$NO_CAPTCHA" -eq 1 ]]; then
  ARGS+=(-pool-size 0)
else
  ARGS+=(-pool-size "$POOL_SIZE" -pool-workers "$POOL_WORKERS")
  if [[ "$HARNESS" -eq 1 ]]; then
    ARGS+=(-captcha-harness -pool-batch "$BATCH")
  fi
fi
if [[ -n "$PROXY" ]]; then
  ARGS+=(-proxy "$PROXY")
fi
if [[ -n "$CHAMPION_BUDGET" ]]; then
  ARGS+=(-champion-budget "$CHAMPION_BUDGET")
fi

if [[ "$CLAUDE" -eq 0 ]]; then
  exec "$EXE" "${ARGS[@]}"
fi

if [[ -z "${GNVIPI_GATEWAY_TOKEN:-}" ]]; then
  echo "GNVIPI_GATEWAY_TOKEN must be set when using --claude" >&2
  exit 1
fi

"$EXE" "${ARGS[@]}" &
SERVE_PID=$!
cleanup() {
  if kill -0 "$SERVE_PID" >/dev/null 2>&1; then
    kill "$SERVE_PID" >/dev/null 2>&1 || true
    wait "$SERVE_PID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

READY=0
for _ in $(seq 1 60); do
  if ! kill -0 "$SERVE_PID" >/dev/null 2>&1; then
    break
  fi
  if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 0.5
done

if [[ "$READY" -ne 1 ]]; then
  echo "gateway did not become healthy on port $PORT - skipping claude" >&2
  exit 1
fi

ANTHROPIC_BASE_URL="http://localhost:$PORT" \
ANTHROPIC_AUTH_TOKEN="$GNVIPI_GATEWAY_TOKEN" \
ANTHROPIC_API_KEY="" \
claude
