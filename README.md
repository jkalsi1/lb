# go-lb

A Layer-7 HTTP load balancer written in Go, with pluggable routing strategies, a per-backend circuit breaker, and graceful shutdown.

## Features

- **Round-robin** and **least-connections** routing strategies
- **Circuit breaker** per backend (Closed → Open → Half-Open → Closed)
- **Per-backend connection pooling** to eliminate TIME_WAIT churn under high concurrency
- **Retry logic** — up to 3 retries per backend before marking it failed
- **Background health checks** every 2 minutes via TCP dial
- **Graceful shutdown** — drains in-flight requests before exiting (30 s window)

## Usage

```bash
# build
go build -o lb ./cmd/lb

# run with two backends, least-connections strategy (default)
./lb -backends=http://localhost:8081,http://localhost:8082 -port=3030

# round-robin
./lb -backends=http://localhost:8081,http://localhost:8082 -port=3030 -strategy=roundrobin
```

### Flags

| Flag        | Default      | Description                                    |
|-------------|--------------|------------------------------------------------|
| `-backends` | *(required)* | Comma-separated list of backend URLs           |
| `-port`     | `3030`       | Port the load balancer listens on              |
| `-strategy` | `leastconn`  | Routing strategy: `roundrobin` or `leastconn`  |

## Architecture

```
            ┌─────────────────────────────────────────┐
Client ───► │             lb (HTTP handler)            │
            │                                         │
            │   strategy.Next(backends)               │
            │       ├── RoundRobin                    │
            │       └── LeastConnections              │
            │                                         │
            │   per-backend circuit breaker           │
            │       Closed ──► Open ──► HalfOpen      │
            │          └────────────────┘             │
            └─────────────────────────────────────────┘
                      │          │          │
                  Backend 1  Backend 2  Backend 3
               (ReverseProxy, connection pool, circuit state)
```

### Circuit Breaker

Each backend runs its own circuit breaker independently.

| State      | Behaviour                                                                 |
|------------|---------------------------------------------------------------------------|
| `Closed`   | Normal — requests are forwarded                                           |
| `Open`     | Tripped — requests are immediately rejected (503); reopens after 5 s      |
| `HalfOpen` | One probe request is allowed; success closes the circuit, failure reopens |

**Thresholds (tunable via source):**

| Variable            | Value | Meaning                                          |
|---------------------|-------|--------------------------------------------------|
| `FAILURE_THRESHOLD` | `20`  | Consecutive proxy errors before tripping         |
| `FAILURE_TIMEOUT`   | `5 s` | How long the circuit stays open before probing   |

### Connection Pooling

The reverse proxy for each backend uses a dedicated `http.Transport`:

```
MaxIdleConns:        1000
MaxIdleConnsPerHost: 300
IdleConnTimeout:     90 s
```

Without this, Go's `DefaultTransport` keeps only 2 idle connections per host.
At 200 concurrency, ~198 connections close after every request wave, entering
TIME_WAIT (30 s on macOS). Three back-to-back load test runs exhaust the
process's file-descriptor limit (macOS default: 256), causing `ENOTCONN` errors.
With the pool sized to match concurrency, connections are reused and TIME_WAIT
never accumulates.

## Test Results

All 16 tests pass on Apple M1 (darwin/arm64, Go 1.24).

```
--- PASS: TestRoundRobinDistribution          (0.08s)   999 requests → 333/333/333 per backend
--- PASS: TestLeastConnectionsRoutesToFastestBackend (0.20s)   slow A got 14/60, fast B+C got 46/60
--- PASS: TestHealthCheckRecovery             (0.01s)   backend removed on fail, restored on recovery
--- PASS: TestLatencyPercentiles              (11.83s)  p50=12ms  p95=21ms  p99=22ms
--- PASS: TestGetLeastConnectedPeer_*         (5 cases)
--- PASS: TestGetNextPeer_*                   (5 cases)
--- PASS: TestRoundRobinStrategy              (0.00s)
--- PASS: TestLeastConnectionsStrategy        (0.00s)
--- PASS: TestStrategySkipsDeadBackends       (0.00s)
--- PASS: TestGracefulShutdown                (0.31s)
```

### Routing Layer Benchmark

The benchmark isolates pure LB overhead (strategy selection + two atomic counter
ops + `httputil.ReverseProxy` header copying) using a no-op transport that never
opens real connections.

```
BenchmarkLBThroughput-8   2,847,484 iterations   4,279 ns/op   ~234,000 req/sec
```

This is the **ceiling of the routing layer itself** — the maximum throughput if
backends responded instantly. Real-world throughput is bounded by backend
latency and concurrency.

## Practical Capacity (observed)

Measured with `hey` against two `quickserver` backends on localhost (Apple M1):

```
hey -n 500000 -c 200 http://localhost:3030/
```

| Metric              | Result         |
|---------------------|----------------|
| Total requests      | 500,000        |
| Errors              | 0              |
| Throughput          | ~17,855 req/s  |
| p50 latency         | 10.8 ms        |
| p95 latency         | 21.6 ms        |
| p99 latency         | 30.9 ms        |
| Slowest             | 77.3 ms        |

The gap between the benchmark ceiling (~234k req/s) and the observed throughput
(~18k req/s) is expected: in the live test, each request makes a real round-trip
over the loopback interface to a backend, serialises a response body, and flows
through the full HTTP/1.1 stack on both sides. The routing layer itself consumes
roughly 4 µs per request; the remaining ~52 µs is backend + network time.

### Theoretical Maximum

The routing layer adds approximately **4 µs per request** of overhead
(strategy selection is a single atomic increment for round-robin, or an O(n)
scan over backends for least-connections). Practical throughput is therefore
limited by:

1. **Backend latency × concurrency** — at 200 concurrent connections and 11 ms
   average backend latency, Little's Law gives a ceiling of
   `200 / 0.011 ≈ 18,000 req/s`, matching the observed result exactly.
2. **Connection pool depth** — each backend's `MaxIdleConnsPerHost: 300` supports
   up to 300 concurrent in-flight requests before new connections must be opened.
3. **File descriptors** — at concurrency C with N backends, the LB holds
   `C` incoming FDs + up to `C` outgoing FDs. Ensure `ulimit -n ≥ 2 × C + headroom`.

To increase throughput: add backends (scales linearly) or increase `-c` on the
client side until backend CPU is saturated. The LB itself is not the bottleneck.
