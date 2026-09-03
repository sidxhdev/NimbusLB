# NimbusLB

A concurrent HTTP load balancer in Go built on `net/http` and `httputil.ReverseProxy`, with thread-safe backend pooling, round-robin balancing, active health checks, automatic failover, and full OpenTelemetry observability (Prometheus metrics, distributed traces, and a prebuilt Grafana dashboard).

## Features

- **Reverse-proxy routing** — distributes incoming traffic across multiple backends using `net/http/httputil`
- **Thread-safe backend pool** — atomic round-robin counter + `RWMutex`-guarded registry; race-tested with `go test -race`
- **Round-robin balancing** — lock-free peer selection that skips unhealthy backends
- **Active health checks** — periodic `GET /healthz` probes mark backends alive/dead; automatic recovery on the next successful probe
- **Automatic failover** — connection errors *and* upstream 5xx responses demote the backend and transparently retry on the next healthy peer (configurable retries, buffered responses so clients never see a failing upstream; `503` only when all backends are down)
- **OpenTelemetry observability**
  - Prometheus metrics: request latency histogram, request/error counters, in-flight gauge, per-backend health gauge
  - Distributed traces exported over OTLP gRPC (OTel Collector → Jaeger), with instrumented proxy transports
  - Grafana dashboard provisioned out of the box (latency percentiles, RPS, error rate, backend health)
- **Graceful shutdown** — drains in-flight requests on `SIGINT`/`SIGTERM`

## Architecture

```
                        ┌──────────────────────────────────────────────┐
                        │                 NimbusLB                     │
   client ─────────────▶│  balancer handler (buffered failover)        │
                        │    │                                         │
                        │    ▼                                         │
                        │  ServerPool (round-robin, thread-safe)       │
                        │    │                                         │
                        │    ▼                                         │
                        │  ReverseProxy ── retry/failover on error ──▶ │──▶ backend 1
                        │    │                                         │──▶ backend 2
                        │  HealthChecker (background probes)           │──▶ backend 3
                        │    │                                         │
                        │    ▼                                         │
                        │  OTel SDK ── metrics ─▶ /metrics (Prometheus)│
                        │           └─ traces ─▶ OTLP Collector ─▶ Jaeger
                        └──────────────────────────────────────────────┘
```

Failover behavior:

| Failure mode                    | Detection                        | Result for the client                        |
|---------------------------------|----------------------------------|----------------------------------------------|
| Backend down (conn refused)     | proxy `ErrorHandler`             | transparent retry on next healthy peer       |
| Backend alive but returns 5xx   | buffered response inspection     | backend demoted, transparent retry           |
| Backend recovers                | next active health probe succeeds| rejoins round-robin rotation automatically   |
| All backends down               | pool scan finds no healthy peer  | `503 Service Unavailable`                    |

## Quickstart

### Full stack (Docker Compose)

Brings up the load balancer, 3 demo backends, OTel Collector, Jaeger, Prometheus, and Grafana:

```bash
docker compose -f deployments/docker-compose.yml up --build
```

Then:

| Service       | URL                                 |
|---------------|-------------------------------------|
| Load balancer | http://localhost:8080               |
| Metrics       | http://localhost:9090/metrics       |
| Prometheus    | http://localhost:9091               |
| Grafana       | http://localhost:3000 (admin/admin) |
| Jaeger UI     | http://localhost:16686              |

```bash
# verify round-robin distribution
for i in $(seq 1 6); do curl -s http://localhost:8080/; done
# → hello from backend-1, backend-2, backend-3, backend-1, ...
```

### Local (Go)

```bash
# start demo backends (separate terminals)
go run ./cmd/backend -port 8081 -id backend-1
go run ./cmd/backend -port 8082 -id backend-2
go run ./cmd/backend -port 8083 -id backend-3

# start the load balancer
go run ./cmd/nimbuslb -config config.yaml
```

## Configuration

`config.yaml`:

```yaml
port: 8080               # listener for proxied traffic
metrics_port: 9090       # Prometheus /metrics listener
health_interval: 10s     # active health-check cadence
health_timeout: 5s       # per-probe timeout
retries: 3               # failover attempts per request
otel_endpoint: ""        # OTLP gRPC endpoint, e.g. localhost:4317 (empty = tracing disabled)
backends:
  - http://localhost:8081
  - http://localhost:8082
  - http://localhost:8083
```

CLI flags override the file: `-config`, `-port`, `-metrics-port`, `-backends url1,url2`, `-hc-interval`, `-retries`.

## Observability

**Metrics** (Prometheus, exposed at `:9090/metrics`):

| Metric                  | Type          | Labels                          | Description                     |
|-------------------------|---------------|---------------------------------|---------------------------------|
| `lb_request_duration_ms`| Histogram     | backend, method, path, status   | proxied request latency         |
| `lb_requests_total`     | Counter       | backend, method, path, status   | total proxied requests          |
| `lb_errors_total`       | Counter       | backend, error_type             | proxy/backend errors            |
| `lb_requests_inflight`  | UpDownCounter | —                               | in-flight requests              |
| `lb_backend_health`     | Gauge         | backend                         | 1 = healthy, 0 = down           |

**Traces**: `lb.request` span per incoming request plus `otelhttp`-instrumented proxy transport spans; exported via OTLP gRPC to the collector (forwarded to Jaeger in the compose stack).

**Grafana**: pre-provisioned dashboard (`NimbusLB Overview`) with per-backend RPS, p50/p95/p99 latency, error rate by type, in-flight requests, and live backend health status.

## Demo: automatic failover

```bash
# 1. watch traffic spread across all 3 backends
for i in $(seq 1 6); do curl -s http://localhost:8080/; done

# 2. make backend-1 return 500s (alive but broken)
curl -X POST http://localhost:8081/fail

# 3. traffic shifts to backend-2/3 with zero client errors —
#    the 5xx attempt is demoted and retried on a healthy peer

# 4. hard-kill backend-2 — conn-refused failover, still zero client errors
docker compose -f deployments/docker-compose.yml stop backend-2

# 5. watch backend health in Prometheus/Grafana
curl -s http://localhost:9090/metrics | grep lb_backend_health

# 6. recover — the next health probe adds it back to rotation
curl -X POST http://localhost:8081/recover
docker compose -f deployments/docker-compose.yml start backend-2
```

## Development

```bash
make build        # build binaries to ./bin
make test         # go test -race ./...
make vet          # go vet ./...
make run          # run the load balancer with config.yaml
make compose      # docker compose up the full stack
make compose-down # tear the stack down
```

## Project structure

```
cmd/nimbuslb/       load balancer entrypoint (wiring, graceful shutdown)
cmd/backend/        demo backend (with /fail and /recover toggles)
internal/backend/   Backend + thread-safe ServerPool (round-robin)
internal/balancer/  proxy handler, buffered response failover
internal/health/    active health checker
internal/config/    YAML config + CLI overrides
internal/telemetry/ OTel traces/metrics setup, Prometheus exporter
deployments/        docker-compose, Dockerfiles, Prometheus, OTel Collector, Grafana provisioning
```

## Skills demonstrated

**Languages & frameworks**
- Go — goroutines, `sync.RWMutex`, `sync/atomic`, `context` propagation, graceful shutdown
- `net/http` & `httputil` — reverse proxies, custom `http.Handler`/`ResponseWriter`, `ErrorHandler`/`Director` hooks

**Concurrency & systems design**
- Lock-free round-robin peer selection (atomic counter), race-tested (`go test -race`)
- Fault tolerance: bounded retries, passive failure marking + active health recovery, buffered-response failover (zero client-visible errors on backend 5xx/connection loss)

**Observability (OpenTelemetry)**
- Prometheus metrics (histograms, counters, gauges, up-down counters), RED-method instrumentation
- Distributed tracing over OTLP gRPC, OTel Collector pipelines, Jaeger
- Grafana provisioning (datasources + dashboard JSON), PromQL (latency percentiles, error rates)

**Infrastructure & DevOps**
- Docker multi-stage builds, Docker Compose multi-service orchestration (LB, backends, collector, Jaeger, Prometheus, Grafana)
- YAML config + CLI flag overrides, table-driven tests with `httptest`

## License

MIT
