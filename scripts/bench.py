#!/usr/bin/env python3
"""Benchmark lgllm against a local mock upstream.

Measures the numbers that matter for a lightweight proxy: binary size, cold
start, idle and streaming RSS, request latency (p50/p99), the overhead added
compared with calling the upstream directly, and throughput with a few clients.

Only the Python standard library is used. This script is a development tool, not
part of the runtime.

Usage:
    python3 scripts/bench.py                 # markdown table on stdout
    python3 scripts/bench.py --json out.json # also write machine-readable data
    python3 scripts/bench.py --requests 500 --concurrency 32
"""

from __future__ import annotations

import argparse
import http.client
import json
import os
import shutil
import signal
import socket
import statistics
import subprocess
import sys
import tempfile
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BINARY = ROOT / "bin" / "lgllm"


class MockUpstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    disable_nagle_algorithm = True

    def log_message(self, *args):  # silence
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            body = {}

        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Connection", "close")
            self.end_headers()
            for i in range(8):
                event = {
                    "id": "mock-stream",
                    "object": "chat.completion.chunk",
                    "model": "mock-model",
                    "choices": [{
                        "index": 0,
                        "delta": {"content": "tok%d " % i},
                        "finish_reason": None,
                    }],
                }
                self.wfile.write(("data: " + json.dumps(event) + "\n\n").encode())
                self.wfile.flush()
            final = {
                "id": "mock-stream",
                "object": "chat.completion.chunk",
                "model": "mock-model",
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 8, "completion_tokens": 8, "total_tokens": 16},
            }
            self.wfile.write(("data: " + json.dumps(final) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            return

        payload = json.dumps({
            "id": "mock",
            "object": "chat.completion",
            "model": "mock-model",
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "pong"},
                "finish_reason": "stop",
            }],
            "usage": {"prompt_tokens": 8, "completion_tokens": 1, "total_tokens": 9},
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def free_port(start: int) -> int:
    port = start
    while port < start + 50:
        with socket.socket() as s:
            try:
                s.bind(("127.0.0.1", port))
                return port
            except OSError:
                port += 1
    raise RuntimeError("no free port")


def rss_kb(pid: int) -> int:
    try:
        for line in Path("/proc/%d/status" % pid).read_text().splitlines():
            if line.startswith("VmRSS:"):
                return int(line.split()[1])
    except OSError:
        pass
    try:
        out = subprocess.check_output(["ps", "-o", "rss=", "-p", str(pid)])
        return int(out.strip() or 0)
    except (subprocess.CalledProcessError, OSError, ValueError):
        return 0


def connect(port: int) -> http.client.HTTPConnection:
    return http.client.HTTPConnection("127.0.0.1", port, timeout=10)


def request_on(conn: http.client.HTTPConnection, stream: bool = False) -> float:
    body = json.dumps({
        "model": "mock/mock-model",
        "messages": [{"role": "user", "content": "ping"}],
        "stream": stream,
    }).encode()
    start = time.perf_counter()
    conn.request("POST", "/v1/chat/completions", body, {"Content-Type": "application/json"})
    resp = conn.getresponse()
    data = resp.read()
    elapsed = time.perf_counter() - start
    if resp.status != 200:
        raise RuntimeError("status %d: %r" % (resp.status, data[:200]))
    return elapsed


def one_request(port: int, stream: bool = False) -> float:
    conn = connect(port)
    try:
        return request_on(conn, stream)
    finally:
        conn.close()


def direct_request_on(conn: http.client.HTTPConnection) -> float:
    body = json.dumps({
        "model": "mock-model",
        "messages": [{"role": "user", "content": "ping"}],
    }).encode()
    start = time.perf_counter()
    conn.request("POST", "/chat/completions", body, {"Content-Type": "application/json"})
    resp = conn.getresponse()
    resp.read()
    elapsed = time.perf_counter() - start
    if resp.status != 200:
        raise RuntimeError("direct status %d" % resp.status)
    return elapsed


def direct_request(port: int) -> float:
    conn = connect(port)
    try:
        return direct_request_on(conn)
    finally:
        conn.close()


def percentile(values: list[float], q: float) -> float:
    ordered = sorted(values)
    if not ordered:
        return 0.0
    return ordered[min(len(ordered) - 1, int(q * (len(ordered) - 1)))]


def wait_healthy(port: int, timeout: float = 5.0) -> float:
    start = time.perf_counter()
    while time.perf_counter() - start < timeout:
        try:
            conn = http.client.HTTPConnection("127.0.0.1", port, timeout=0.25)
            conn.request("GET", "/healthz")
            resp = conn.getresponse()
            resp.read()
            conn.close()
            if resp.status == 200:
                return (time.perf_counter() - start) * 1000
        except OSError:
            pass
        time.sleep(0.002)
    raise RuntimeError("lgllm did not become healthy in %.1fs" % timeout)


def write_config(path: Path, proxy_port: int, upstream_port: int) -> None:
    path.write_text(f"""server:
  listen: 127.0.0.1:{proxy_port}
  read_header_timeout: 10s
  idle_timeout: 120s
  shutdown_timeout: 5s
auth:
  mode: off
log:
  level: error
  format: json
providers:
  - id: mock
    base_url: http://127.0.0.1:{upstream_port}
    format: openai
    auth: none
    models:
      - id: mock-model
connections:
  - id: mock-conn
    provider: mock
metrics:
  enabled: false
tokensaver:
  enabled: false
""")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--requests", type=int, default=300, help="sequential requests (default 300)")
    ap.add_argument("--concurrency", type=int, default=16, help="concurrent clients (default 16)")
    ap.add_argument("--per-client", type=int, default=20, help="requests per concurrent client")
    ap.add_argument("--stream-requests", type=int, default=20)
    ap.add_argument("--json", type=Path, help="write results as JSON")
    ap.add_argument("--skip-build", action="store_true")
    args = ap.parse_args()

    if not args.skip_build:
        subprocess.run(["make", "build"], cwd=ROOT, check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT)
    if not BINARY.exists():
        print("missing %s; run make build" % BINARY, file=sys.stderr)
        return 1

    binary_bytes = BINARY.stat().st_size
    upstream_port = free_port(39391)
    proxy_port = free_port(39381)

    server = ThreadingHTTPServer(("127.0.0.1", upstream_port), MockUpstream)
    server.daemon_threads = True
    threading.Thread(target=server.serve_forever, daemon=True).start()

    tmp = Path(tempfile.mkdtemp(prefix="lgllm-bench-"))
    cfg = tmp / "config.yaml"
    write_config(cfg, proxy_port, upstream_port)
    log = open(tmp / "lgllm.log", "wb")
    proc = subprocess.Popen(
        [str(BINARY), "-config", str(cfg), "-config-poll", "0"],
        stdout=log, stderr=subprocess.STDOUT,
    )

    results: dict[str, object] = {}
    try:
        results["cold_start_ms"] = round(wait_healthy(proxy_port), 1)
        results["rss_idle_kb"] = rss_kb(proc.pid)

        warm = connect(proxy_port)
        for _ in range(5):
            request_on(warm)
        proxy_latencies = [request_on(warm) for _ in range(args.requests)]
        warm.close()

        direct_conn = connect(upstream_port)
        direct_latencies = [direct_request_on(direct_conn) for _ in range(min(args.requests, 100))]
        direct_conn.close()

        results["latency_p50_ms"] = round(percentile(proxy_latencies, 0.50) * 1000, 2)
        results["latency_p95_ms"] = round(percentile(proxy_latencies, 0.95) * 1000, 2)
        results["latency_p99_ms"] = round(percentile(proxy_latencies, 0.99) * 1000, 2)
        results["direct_p50_ms"] = round(percentile(direct_latencies, 0.50) * 1000, 2)
        results["overhead_p50_ms"] = round(
            (percentile(proxy_latencies, 0.50) - percentile(direct_latencies, 0.50)) * 1000, 2)

        def worker() -> int:
            ok = 0
            conn = connect(proxy_port)
            for _ in range(args.per_client):
                try:
                    request_on(conn)
                    ok += 1
                except OSError:
                    conn.close()
                    conn = connect(proxy_port)
            conn.close()
            return ok

        started = time.perf_counter()
        with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
            counts = list(pool.map(lambda _: worker(), range(args.concurrency)))
        elapsed = time.perf_counter() - started
        total = sum(counts)
        results["throughput_rps"] = round(total / elapsed, 1)
        results["throughput_requests"] = total

        for _ in range(3):
            one_request(proxy_port, stream=True)
        results["rss_streaming_kb"] = rss_kb(proc.pid)
        stream_started = time.perf_counter()
        for _ in range(args.stream_requests):
            one_request(proxy_port, stream=True)
        results["stream_avg_ms"] = round(
            (time.perf_counter() - stream_started) / args.stream_requests * 1000, 2)
        results["rss_streaming_kb"] = max(results["rss_streaming_kb"], rss_kb(proc.pid))

        image_size = None
        if shutil.which("docker"):
            print("note: docker found; build the image separately with `make image-size`",
                  file=sys.stderr)

        results["binary_bytes"] = binary_bytes
        results["binary_mb"] = round(binary_bytes / (1024 * 1024), 2)
        results["image_size_note"] = image_size
        results["go_version"] = subprocess.check_output(["go", "version"], text=True).strip()
        results["cpu"] = cpu_model()
        results["requests"] = args.requests
        results["concurrency"] = args.concurrency

        print_table(results)
        if args.json:
            args.json.write_text(json.dumps(results, indent=2) + "\n")
    finally:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
        server.shutdown()
        log.close()
        shutil.rmtree(tmp, ignore_errors=True)
    return 0


def cpu_model() -> str:
    try:
        for line in Path("/proc/cpuinfo").read_text().splitlines():
            if line.startswith("model name"):
                return line.split(":", 1)[1].strip()
    except OSError:
        pass
    return "unknown"


def print_table(r: dict[str, object]) -> None:
    print("| Metric | Value |")
    print("|---|---|")
    print(f"| Binary size | {r['binary_mb']} MB |")
    print(f"| Cold start (process → /healthz) | {r['cold_start_ms']} ms |")
    print(f"| RSS idle | {r['rss_idle_kb'] / 1024:.1f} MB |")
    print(f"| RSS while streaming | {r['rss_streaming_kb'] / 1024:.1f} MB |")
    print(f"| Latency p50 (proxy) | {r['latency_p50_ms']} ms |")
    print(f"| Latency p95 (proxy) | {r['latency_p95_ms']} ms |")
    print(f"| Latency p99 (proxy) | {r['latency_p99_ms']} ms |")
    print(f"| Latency p50 (upstream direct) | {r['direct_p50_ms']} ms |")
    print(f"| Proxy overhead p50 | {r['overhead_p50_ms']} ms |")
    print(f"| Throughput ({r['concurrency']} clients) | {r['throughput_rps']} req/s |")
    print(f"| Streaming avg | {r['stream_avg_ms']} ms |")
    print()
    print(f"_{r['go_version']}; CPU: {r['cpu']}; sequential n={r['requests']}_")


if __name__ == "__main__":
    raise SystemExit(main())