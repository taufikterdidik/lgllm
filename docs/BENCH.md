# Benchmark

Numbers from `python3 scripts/bench.py` (or `make bench`) against a local mock
upstream. The mock is a plain `http.server` returning a small chat completion;
the client keeps connections alive, so the figures describe proxy overhead rather
than network or provider latency.

## Result (2026-11-07)

| Metric | Value |
|---|---|
| Binary size (`bin/lgllm`, `-s -w -trimpath`) | **8.97 MB** |
| Container image (`scratch`, compressed layers) | **8.7 MB** |
| Cold start (process → first successful `/healthz`) | **5.1 ms** |
| RSS idle | **13.9 MB** |
| RSS while streaming | **20.0 MB** |
| Latency p50 (proxy, 500 requests) | **0.39 ms** |
| Latency p95 | **0.55 ms** |
| Latency p99 | **0.78 ms** |
| Latency p50 (upstream direct) | **0.24 ms** |
| Proxy overhead p50 | **0.16 ms** |
| Throughput (32 clients, keep-alive) | **2398 req/s** |
| Streaming request average (8 events) | **0.98 ms** |

Environment: Go 1.27.3, Linux amd64, Intel Core i3-4330 @ 3.50 GHz, loopback
networking, mock upstream on the same host.

The binary is measured after `make build`; the image after `make image-size`
(`docker build` of the included Dockerfile). The container was also smoke-tested:
`/healthz` answers `{"status":"ok"}` when run with a read-only mounted config.

## What the numbers mean

- **Cold start & RSS** are the lightweight-binary goals: a single static file,
  no runtime, and a small idle footprint.
- **Overhead** is the difference between the proxy's and the direct upstream's
  p50. It excludes the mock's own processing time and is the cost of routing,
  header handling, and the relay loop.
- **Throughput** is intentionally modest hardware and a Python mock; use it as a
  regression baseline, not a capacity plan.
- Streaming RSS is sampled after a few SSE requests; it is not a soak test.

## Reproducing

```bash
make build
python3 scripts/bench.py --requests 500 --concurrency 32 --per-client 40
python3 scripts/bench.py --json /tmp/bench.json   # machine-readable
make image-size                                  # needs docker
```

The script starts its own mock upstream, writes a temporary config, starts
`bin/lgllm`, measures, and shuts everything down. It uses only the Python
standard library and is not part of the runtime.

## Comparing with 9Router

The comparison is not automated here because 9Router needs its own config and
credentials. To compare on the same machine:

1. Run 9Router on one port and lgllm on another, both pointing at the same
   upstream (or at the same mock).
2. Measure size (`docker image inspect`), idle RSS (`ps -o rss=`), cold start
   (time to `/healthz`), and latency with the same client script and request
   body.
3. Keep the client connections alive for both; a new TCP connection per request
   measures the kernel, not the proxy.

The figures above are for lgllm only; they are a baseline for phase 11, not a
claim about another implementation.