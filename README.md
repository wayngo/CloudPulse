# Cloudpulse — Global Health Monitor

A production-grade Go microservice that pings a list of URLs concurrently and reports their uptime status and response latency. It ships with structured JSON logging, OpenTelemetry-instrumented Prometheus metrics, and a multi-stage Docker build that produces a sub-50 MB image.

This is **Phase 1: Core App Development.** Infrastructure (Kubernetes manifests, Helm charts, CI, distributed tracing, etc.) is intentionally out of scope and will be added in a follow-up phase.

---

## Features

- **POST `/check`** — accepts a JSON list of URLs and returns each URL's status (`up`/`down`), HTTP status code, latency in milliseconds, and any error.
- **Concurrent fan-out** — one goroutine per URL coordinated by a `sync.WaitGroup`; each goroutine writes to its own pre-allocated slot in the result slice (no mutex required).
- **Structured logging** — Uber [zap](https://github.com/uber-go/zap) production logger emitting JSON to stdout.
- **OpenTelemetry metrics** — `total_pings_count` (counter, labelled by `status`) and `latency_histogram` (in ms), exported via the Prometheus exporter on `GET /metrics`.
- **Liveness probe** — `GET /healthz` returning `{"status":"ok"}`.
- **Graceful shutdown** — handles `SIGINT`/`SIGTERM`, drains in-flight requests, and flushes the meter provider.
- **Hardened container** — distroless `nonroot` runtime image, fully static `CGO_ENABLED=0` binary, **~28 MB** final image.

---

## Architecture

```
                            +------------------+
       POST /check          |                  |   one goroutine per URL
   ----------------------->|   Gin Router     |--------------------------+
                            |  (main.go)       |                          |
                            +--------+---------+                          v
                                     |                          +-------------------+
                              GET /metrics                       |  monitor.Pinger   |
                                     |                          |  (internal/       |
                                     v                          |   monitor)        |
                            +------------------+                +---------+---------+
                            | Prometheus       |                          |
                            | exporter handler |<-----+                   | http.Client
                            +------------------+      |                   v
                                                      |          External targets
                            +------------------+      |                   |
                            |  OTel SDK        |<-----+                   |
                            |  MeterProvider   |  records every ping      |
                            |  (internal/      |  (counter + histogram)   |
                            |   metrics)       |                          |
                            +------------------+<-------------------------+
```

### Project layout

```
cloudpulse/
├── main.go                        # Gin setup, routes, graceful shutdown
├── main_test.go                   # Unit tests (httptest mocks, no real network)
├── go.mod / go.sum
├── Dockerfile                     # Multi-stage: golang:1.25-alpine -> distroless/static
├── .dockerignore
├── .gitignore
└── internal/
    ├── monitor/
    │   └── monitor.go             # Pinger, CheckURLs, CheckResult, classifyError
    └── metrics/
        └── metrics.go             # OTel MeterProvider + Prometheus exporter + instruments
```

`internal/...` enforces that these packages can only be imported by code inside this module, which is the convention for non-public domain logic.

---

## API

### `POST /check`

**Request**

```json
{
  "urls": [
    "https://example.com",
    "https://google.com",
    "http://does-not-exist.invalid"
  ]
}
```

The `urls` field is required and must be non-empty; each entry must be a non-empty string. Validation failures return `400 Bad Request`.

**Response — `200 OK`**

```json
{
  "results": [
    { "url": "https://example.com",           "status": "up",   "status_code": 200, "latency_ms": 142 },
    { "url": "https://google.com",            "status": "up",   "status_code": 200, "latency_ms": 87  },
    { "url": "http://does-not-exist.invalid", "status": "down", "latency_ms": 12, "error": "dial tcp: lookup does-not-exist.invalid: no such host" }
  ]
}
```

- A target is `up` when it responds with a 2xx or 3xx status.
- Anything else (4xx, 5xx, transport errors, DNS failures, timeouts) is `down`.
- `latency_ms` is wall-clock time around `client.Do(req)`.

### `GET /healthz`

Cheap liveness probe — returns `{"status":"ok"}`.

### `GET /metrics`

Prometheus scrape endpoint exposing:

| Metric                            | Type      | Labels                  | Description                              |
| --------------------------------- | --------- | ----------------------- | ---------------------------------------- |
| `total_pings_count`               | Counter   | `status="up"\|"down"`   | Total URL pings performed.               |
| `latency_histogram_milliseconds`  | Histogram | `status="up"\|"down"`   | Distribution of ping latencies.          |

(Plus the standard `go_*` and process metrics that the Prometheus client adds automatically. The OTel→Prometheus translator appends the unit suffix `_milliseconds` to the histogram name.)

---

## Prerequisites

To build and run from a fresh clone you need:

| Tool                | Version                | Why                                                                                              |
| ------------------- | ---------------------- | ------------------------------------------------------------------------------------------------ |
| **Go**              | **1.25 or newer**      | The module declares `go 1.25.0` because a transitive dependency requires it. Install from <https://go.dev/dl/>. |
| **Git**             | any recent             | To clone the repo.                                                                               |
| **Docker** *(opt.)* | 20.10+ with BuildKit   | Only required if you want to build/run the container image.                                      |
| **curl** *(opt.)*   | any                    | For poking the API from a terminal. PowerShell users can use `Invoke-RestMethod` instead.        |

That's it — no databases, no message brokers, no external services to spin up.

### Verifying your toolchain

```bash
go version          # should print >= go1.25
git --version
docker --version    # only if you plan to build the image
```

---

## Running locally

### 1. Clone and fetch dependencies

```bash
git clone https://github.com/wayne-ngo/cloudpulse.git
cd cloudpulse
go mod download
```

### 2. Run the server

```bash
go run .
```

You should see structured JSON logs like:

```json
{"level":"info","ts":1746162000.123,"caller":"cloudpulse/main.go:88","msg":"server listening","addr":":8080"}
```

By default it binds to `:8080`. Override with the `CLOUDPULSE_ADDR` environment variable:

```bash
# Linux / macOS
CLOUDPULSE_ADDR=":9090" go run .

# Windows PowerShell
$env:CLOUDPULSE_ADDR=":9090"; go run .
```

### 3. Send a request

```bash
curl -s http://localhost:8080/check \
  -H 'Content-Type: application/json' \
  -d '{"urls":["https://example.com","https://google.com"]}' | jq
```

PowerShell equivalent:

```powershell
Invoke-RestMethod -Method POST -Uri http://localhost:8080/check `
  -ContentType 'application/json' `
  -Body '{"urls":["https://example.com","https://google.com"]}'
```

### 4. Scrape metrics

```bash
curl -s http://localhost:8080/metrics | grep -E 'total_pings_count|latency_histogram'
```

### 5. Liveness check

```bash
curl -s http://localhost:8080/healthz
# {"status":"ok"}
```

### Configuration

| Env var            | Default   | Purpose                                          |
| ------------------ | --------- | ------------------------------------------------ |
| `CLOUDPULSE_ADDR`  | `:8080`   | Address/port the HTTP server binds to.           |

The per-request HTTP client timeout is currently a constant (`5s`) in `main.go`; promote it to an env var if you need it tunable.

---

## Testing

```bash
go test ./... -v -count=1
```

The suite lives in `main_test.go` and uses `net/http/httptest` to spin up local mock servers — no real network calls — so it runs in well under a second. Coverage:

| Test                          | What it proves                                                                                                       |
| ----------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `TestCheckURLs_AllUp`         | All 2xx responses report `up` with non-negative latency.                                                             |
| `TestCheckURLs_MixedStatuses` | 200 → up, 500 → down, unreachable host → down with error.                                                            |
| `TestCheckURLs_Timeout`       | Slow target exceeds client timeout → `down` with timeout error.                                                      |
| `TestCheckURLs_Concurrency`   | 10 × 100 ms-delayed pings finish in < 600 ms **and** at least 2 are observed in-flight simultaneously — direct proof of goroutine fan-out. |
| `TestCheckURLs_EmptyInput`    | `nil` and `[]string{}` both return an empty slice without panicking.                                                 |
| `TestCheckURLs_InvalidURL`    | A malformed URL surfaces as `down` with a non-empty error.                                                           |

Run with race detection:

```bash
go test ./... -race
```

---

## Building & running the container

The Dockerfile is a multi-stage build:

1. **Builder stage** — `golang:1.25-alpine`, BuildKit cache mounts for `/go/pkg/mod` and the Go build cache, produces a static binary with `CGO_ENABLED=0` + `-ldflags="-s -w"` + `-trimpath`.
2. **Runtime stage** — `gcr.io/distroless/static-debian12:nonroot`, which provides CA certificates (needed for HTTPS), a non-root user, and tzdata, while staying ~2 MB.

### Build

```bash
docker build -t cloudpulse:dev .
```

### Run

```bash
docker run --rm -p 8080:8080 cloudpulse:dev
```

### Verify image size (must be < 50 MB)

```bash
docker images cloudpulse:dev --format '{{.Size}}'
# observed: ~28 MB (25.6 MB binary + ~2 MB distroless base)
```

The static binary on its own is ~25.6 MB; that's our floor and gives plenty of headroom under the 50 MB ceiling.

---

## What was implemented & how

| Plan requirement                         | Implementation                                                                                                                                                                                                                                                  |
| ---------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Lightweight HTTP framework               | **Gin** (`github.com/gin-gonic/gin v1.12.0`) with `gin.Recovery()` middleware and `gin.ReleaseMode`.                                                                                                                                                            |
| `POST /check` accepting a JSON list of URLs | `CheckRequest` struct in `main.go` with `binding:"required,min=1,dive,required"` validators.                                                                                                                                                                  |
| Uptime status + latency in ms            | `monitor.CheckResult` (`internal/monitor/monitor.go`) — `Status` is `"up"` for 2xx/3xx, otherwise `"down"`; `LatencyMs` is `time.Since(start).Milliseconds()`.                                                                                                  |
| Concurrency via goroutines               | `Pinger.CheckURLs` in `internal/monitor/monitor.go` — fans out one goroutine per URL using `sync.WaitGroup`, with each goroutine writing to its own pre-allocated index in the result slice (no mutex needed).                                                  |
| Structured JSON logging                  | `zap.NewProduction()` in `main.go`, deferred `Sync()`; `Pinger` logs every check at `Info` (success) or `Warn` (failure).                                                                                                                                       |
| OpenTelemetry SDK + custom metrics       | `internal/metrics/metrics.go` — wires the SDK `MeterProvider` to a Prometheus exporter, registers `total_pings_count` (Int64Counter) and `latency_histogram` (Float64Histogram, unit `ms`). Both are recorded inside `Pinger.checkOne`.                          |
| Prometheus scrape endpoint               | `r.GET("/metrics", gin.WrapH(promhttp.Handler()))` in `main.go`.                                                                                                                                                                                                |
| Unit tests with mocking                  | `main_test.go` uses `httptest.NewServer` so no real network calls happen, including a concurrency test that uses an atomic in-flight counter to *directly* prove goroutine fan-out.                                                                              |
| Multi-stage Dockerfile, < 50 MB          | `golang:1.25-alpine` builder → `gcr.io/distroless/static-debian12:nonroot` runtime, `CGO_ENABLED=0`, `-ldflags="-s -w" -trimpath`. Final image ~28 MB.                                                                                                          |
| Graceful shutdown                        | `signal.Notify` on `SIGINT`/`SIGTERM`, `srv.Shutdown(ctx)` with a 10 s deadline, plus `provider.Shutdown(ctx)` to flush metrics.                                                                                                                                |

---

## Roadmap (deferred to the infrastructure phase)

- Kubernetes manifests / Helm chart (Deployment, Service, ServiceMonitor, HPA).
- CI pipeline (lint, vet, test, build, push image).
- OpenTelemetry **tracing** alongside metrics, exporting to an OTel Collector.
- Auth and rate-limiting on `/check`.
- Configurable per-request timeout, max URLs per request, and bounded worker pool for very large lists.
- HEAD vs GET method selection per target.

---

## License

TBD.
