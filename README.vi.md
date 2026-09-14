# gnvipi — Gateway NVIDIA Playground

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
![Platforms](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey)

> English: [README.md](./README.md)

Gateway LLM đa định dạng tự host: phục vụ các model trên **NVIDIA Playground
(deepseek-v4, kimi, glm, nemotron, gpt-oss, …)** cộng thêm bất kỳ **endpoint
OpenAI-compatible / Anthropic tùy chỉnh** nào, tất cả sau một server local —
kèm tự động giải hCaptcha (pool headless Chrome) và trang admin quản lý model
+ thống kê từng request.

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) nhúng trong repo
lo phần dịch định dạng đầu vào; phần của repo này là executor NVIDIA, tự động
hóa captcha và admin console. Gateway **không** kiểm API key đầu vào — chỉ nên
chạy trên localhost.

## Endpoint

| Route | Định dạng |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | OpenAI Responses |
| `POST /v1/messages` | Anthropic Messages (thay thẳng cho Claude Code) |
| `GET /v1/models` | danh mục model (tôn trọng bật/tắt từ admin) |
| `/admin` | admin console (xem dưới) |
| `GET /healthz` | health + trạng thái pool captcha |

## Bắt đầu nhanh

```powershell
# Windows (khuyên dùng): tự build nếu cần, kiểm port, chạy với -auto
.\run.ps1                      # :8080, 3 chrome, pool 6
.\run.ps1 -Port 8081           # instance test
.\run.ps1 -Claude              # bật gateway + mở Claude Code trỏ vào luôn
.\run.ps1 -Harness -Batch 6    # harness captcha RAM thấp, mint theo lô
.\run.ps1 -Proxy socks5://127.0.0.1:1080
```

```bash
# Mọi nền tảng
go run ./cmd/serve -auto -addr :8080
go run ./cmd/serve -auto -pool-workers=2 -coalesce-ms=0 -max-inflight=8
go run ./cmd/serve -captcha "P1_..."   # token tay dùng một lần, không cần browser
```

Trỏ client vào `http://localhost:8080`. Model id lấy từ catalog playground
(`deepseek-ai/deepseek-v4-flash-0731`, `moonshotai/kimi-k3`, …) cộng alias
`<provider>/<model>` tự thêm trong trang admin.

## Admin console — `/admin`

Mỗi tab (`?page=`) một trang, đúng một chức năng:

- **`?page=dashboard`** — thống kê token/request **từ lúc bật server**, cho
  **mọi** đường API: model playground (executor) *và* custom provider
  (middleware passthrough). KPI (requests, req/phút, token vào/ra, lỗi %,
  TTFB, p95 độ trễ, uptime), biểu đồ theo phút có crosshair tooltip, bảng
  theo model (chỉ hiện model có traffic), và **log 200 request gần nhất**
  (thời gian · model · provider · stream · token · độ trễ · TTFB · ok/lỗi),
  sort được từng cột.
- **`?page=models`** — bật/tắt từng model (lưu file), xóa/khôi phục model khỏi
  catalog, thêm/xóa endpoint OpenAI-compatible tùy chỉnh (bao gồm chế độ
  `messages_only` cho upstream bị WAF chặn đường chat/completions).
- **`?page=chromes`** — các Chrome captcha đang chạy: tạm dừng/hồi phục từng
  process, xem trạng thái busy/warm và số lần extract.

## Cơ chế

```
client ──/v1/*──▶ CLIProxyAPI ──▶ nvidia executor ──▶ predict API playground
                    │                    │                    ▲
                    │                    └─ scrapeUsage ──────┤ SSE usage
                    ▼                                        │
              passthrough (custom providers) ────────────────┤
              pool captcha (headless Chrome + hCaptcha) ─────┘ nv-captcha-token
              GlobalStats.Observe ◀── mọi request trên mọi đường
```

- **Pool captcha**: `-auto` prewarm token hCaptcha bằng headless Chrome
  (`internal/captcha`); request lấy token từ pool, token cũ bị recycle
  (`-pool-ttl`); có thể mint theo lô trên harness page local
  (`-captcha-harness -pool-batch=N`) để giảm RAM và thời gian.
- **Gộp SSE**: các delta nội dung liên tiếp gộp trong cửa sổ `-coalesce-ms`
  (mặc định 16) để giảm churn event-loop; token đầu luôn flush ngay nên TTFT
  không đổi.
- **Claude Code**: bật/tắt model từ `/admin` tự ghi lại cache model của gateway
  nên `/model` phản ánh catalog không cần login lại; `.\run.ps1 -Claude` tự set
  `ANTHROPIC_BASE_URL`.

## Flag đáng chú ý

```
-auto                     prewarm pool captcha (headless Chrome)
-pool-size/-pool-workers  độ sâu pool / số Chrome song song
-captcha-harness          mint trên trang trắng local (~50MB/chrome, batch được)
-pool-batch N             token mỗi lần mượn browser (chỉ harness)
-coalesce-ms N            cửa sổ gộp delta SSE (0=tắt)
-max-inflight N -inflight-wait  giới hạn stream upstream -> 503 khi kín
-captcha-wait D           thời gian chờ token captcha mỗi request
-models-file P            nơi lưu catalog + mask + providers
-captcha-select-budget D  benchmark URL playground, lưu champion
-addr :8080  -chrome-proxy URL
```

## Development

```bash
go build ./... && go test ./...
```

Package chính: `cmd/serve` (gateway + admin), `internal/provider/nvidia`
(executor, gộp/chuẩn hóa SSE, `stats.go`), `internal/captcha` (nhóm browser,
pool, harness, chọn champion), `internal/models` (catalog playground).

Deployment Docker cho browser captcha: `Dockerfile` / `docker-compose.yml`.
