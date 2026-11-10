# TTFT / concurrency experiment notes (2026-07-21)

Measured with warmed captcha pool → local `serve` → NVIDIA predict SSE.
Tooling: `cmd/streambench` (the one-off `scripts/ttft_sweep.sh` was removed in #157).

## Sequential (pool ready ≥ 1)

| config | ttfb_ms runs | notes |
|--------|--------------|-------|
| coalesce0 pool2/w1 | 2793, 10312 | high upstream variance |
| **coalesce16 pool2/w1** | **1615, 694** | best sequential TTFT |
| coalesce16 pool3/w2 | 3607, 2115 | |
| coalesce16 pool2/w2 | 2796, 2031 | |

## Concurrent (−concurrency 2, pool prefilled)

| config | ttfb_ms | wall |
|--------|---------|------|
| pool2/w1 | 2128 / 3443 | 6.7s (uneven) |
| **pool3/w2** | **2356 / 2355** | **2.6s** (even) |

## Cold vs warm (same process)

| state | ttfb_ms |
|-------|---------|
| cold (ready=0) | **14818** |
| warm (ready≥1) | 6306 (upstream-dominated) |

## Defaults chosen

Prioritize first-token latency **and** concurrent fairness:

- `-coalesce-ms=16` (eager first flush; coalesce only after first content)
- `-pool-size=3`
- `-pool-workers=2`
- `-max-inflight=4`
- `-warm-timeout=3m` (block accepting until ≥1 token)

Re-run with `go run ./cmd/streambench` against a local `serve`.
