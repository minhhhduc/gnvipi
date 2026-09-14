You are smoke-testing a Go NVIDIA Playground proxy that lives in this repo.
A local `serve` is already running on http://localhost:8080 with auto-captcha enabled.

Your job is to verify the kimi-k3 end-to-end path with REAL tool calls, not to fix code.

## Step 1 — code orientation
- Run `ls internal/provider/nvidia/` to see the executor files.
- Read `internal/provider/nvidia/coalesce.go` — focus on `coalesceSSEEvents` and `pipeSSELines`. Note: a fix was just added that drops `data: ` lines with empty payloads.
- Read `internal/provider/nvidia/executor.go` `ExecuteStream` to understand how SSE frames flow.

## Step 2 — single-turn smoke (no tools)
- Send one chat completion request via curl:
  ```
  curl -s -m 60 -X POST http://localhost:8080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"moonshotai/kimi-k3","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"Reply with exactly PONG"}]}' \
    | grep -c "reasoning_content"
  ```
- Count `data: [DONE]` occurrences — must equal 1.
- Count lines matching `^data: $` — must equal 0 (empty payload leak).
- Count lines matching `^: ` — must equal 0 (keepalive leak).

## Step 3 — tool call round-trip
- Send a request that forces a tool call (use Bash to run curl, do NOT hand-build JSON):
  - model: `moonshotai/kimi-k3`
  - reasoning_effort: `high`
  - tools: one function `get_weather(city: string)`
  - message: "What is the weather in Tokyo? Use the get_weather tool."
- Use `Bash` to write the request body to a temp file then `curl --data @file`.
- Verify with `grep` that the response contains:
  - `tool_calls` chunk with `function.name = "get_weather"`
  - `function.arguments` parseable as JSON with `city: "Tokyo"`
  - exactly one `data: [DONE]`
  - zero `^data: $` lines

## Step 4 — read the serve log
- Find the serve log. Try in order:
  - `C:\Users\Admin\AppData\Local\Temp\serve.log`
  - the path the user gave in chat, or any `serve*.log` under `/tmp` / `%TEMP%`
- If you cannot find the log, ASK the user for the path — do NOT guess.
- Read the last 30 lines. Look for:
  - error / panic / FATAL lines
  - "captcha browser hard failure" streaks
  - any warning about coalesce or SSE

## Step 5 — report
Write a short table:

| check | expected | actual | pass? |
|---|---|---|---|
| reasoning_chunks | > 0 | … | … |
| content_chunks | >= 1 | … | … |
| tool_call emitted | yes | … | … |
| empty data: leak | 0 | … | … |
| keepalive leak | 0 | … | … |
| [DONE] count | 1 | … | … |
| serve log errors | none | … | … |

If any row fails, include the offending raw bytes / log lines. Otherwise say "ALL PASS".

## Rules
- Use `Bash` and `Read` tools, not a wall of inline `curl`.
- Do NOT modify any source file. This is a smoke test, not a fix.
- Do NOT start or stop the serve. It is already running on :8080.
- If a check fails, do not retry more than once — report the failure and stop.
- Be brief in commentary; let the table speak.
