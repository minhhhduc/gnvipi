#!/usr/bin/env bash
# Live test kimi-k3 via serve on 8080, then dump serve log tail.
# Mirrors the exact request Claude Code would send (single tool call).
set -euo pipefail

HOST="${HOST:-localhost}"
PORT="${PORT:-8080}"
MODEL="${MODEL:-moonshotai/kimi-k3}"
LOG="${LOG:-/tmp/serve.log}"

OUT=$(mktemp)
trap 'rm -f "$OUT"' EXIT

cat >"$OUT.req" <<'JSON'
{
  "model": "moonshotai/kimi-k3",
  "stream": true,
  "reasoning_effort": "high",
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "get_weather",
        "description": "Get current weather for a city",
        "parameters": {
          "type": "object",
          "properties": {
            "city": {"type": "string", "description": "City name"}
          },
          "required": ["city"]
        }
      }
    }
  ],
  "messages": [
    {"role": "user", "content": "What is the weather in Tokyo? Use the get_weather tool."}
  ]
}
JSON

echo "=== request ==="
cat "$OUT.req"
echo
echo "=== streaming response (first 5 + last 5 lines) ==="
curl -sS -m 120 -X POST "http://${HOST}:${PORT}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  --data @"$OUT.req" >"$OUT" || { echo "curl failed: $?"; exit 1; }

TOTAL=$(wc -l <"$OUT")
echo "stream lines: $TOTAL"
echo "--- head ---"
head -5 "$OUT"
echo "--- tail ---"
tail -5 "$OUT"
echo
echo "=== counters ==="
echo "reasoning_chunks : $(grep -c reasoning_content "$OUT")"
echo "content_chunks   : $(grep -c '\"content\":\"' "$OUT")"
echo "tool_call_chunks : $(grep -c tool_calls "$OUT")"
echo "finish_stop      : $(grep -c '\"finish_reason\":\"stop\"' "$OUT")"
echo "finish_tool      : $(grep -c '\"finish_reason\":\"tool_calls\"' "$OUT")"
echo "empty_data_lines : $(grep -c '^data: $' "$OUT")"
echo "keepalive_colons : $(grep -c '^: ' "$OUT")"
echo "DONE_sentinels   : $(grep -c 'data: \[DONE\]' "$OUT")"
echo
echo "=== tool_calls payload (first match) ==="
grep -m1 'tool_calls' "$OUT" | head -c 400; echo
echo
echo "=== usage ==="
grep -m1 '\"usage\"' "$OUT"
echo
echo "=== FAIL conditions ==="
if grep -q '^data: $' "$OUT"; then echo "FAIL: empty data: line leaked"; exit 2; fi
if grep -q '^: ' "$OUT"; then echo "FAIL: keepalive leaked"; exit 2; fi
if [ "$(grep -c 'data: \[DONE\]' "$OUT")" -ne 1 ]; then echo "FAIL: missing [DONE]"; exit 2; fi
if [ "$(grep -c tool_calls "$OUT")" -lt 1 ]; then echo "FAIL: no tool_call emitted"; exit 2; fi
echo "PASS: tool call round-trip clean on serve :${PORT}"
echo
echo "=== serve log tail (${LOG}) ==="
if [ -f "$LOG" ]; then
  tail -15 "$LOG"
else
  echo "(no log file at $LOG; serve may be running outside /tmp)"
fi
