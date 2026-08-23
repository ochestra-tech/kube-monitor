# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build
go mod tidy
go build -o k8s-monitor ./cmd

# Run (one-shot report)
./k8s-monitor --one-shot --type combined --format text

# Run (continuous with HTTP server)
./k8s-monitor --interval 5m --metrics-port 8085

# Test
go test ./...
go test -cover ./...
go test ./pkg/health/    # single package

# Docker
docker build -t k8s-monitor .
docker run k8s-monitor --type health --format text
```

## Environment Variables

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | — | Postgres connection; enables persistent time-series store |
| `RABBITMQ_URL` | — | RabbitMQ broker; enables anomaly event publishing |
| `ANOMALY_RABBITMQ_EXCHANGE` | `k8s.anomalies` | Exchange name for anomaly events |
| `CORS_ALLOWED_ORIGINS` | — | Comma-separated allowlist (never wildcard) |
| `PROMETHEUS_URL` | — | Used by optimizer for network/storage metrics |
| `K8S_MONITOR_GCP_API_KEY` | — | GCP Cloud Billing API key for pricing |

Both `DATABASE_URL` and `RABBITMQ_URL` are fully optional — the service degrades gracefully to in-memory ring buffer and log output respectively.

## Key CLI Flags

`--kubeconfig`, `--interval` (default 60s), `--metrics-port` (default 8085), `--one-shot`, `--type` (health/cost/combined), `--format` (text/json/html), `--cluster-id`, `--request-timeout` (default 90s), `--anomaly-window` (default 60), `--anomaly-z-threshold` (default 2.5), `--ring-buffer-capacity` (default 1000), `--enable-detailed-metrics`, `--pricing-debug`.

## Architecture

### Hexagonal layout

```
cmd/main.go           ← entrypoint, CLI flags, HTTP server, report loop
internal/
  ports/              ← interfaces: TimeSeriesStore, Cache, PricingProvider, ReportGenerator, HealthChecker
  domain/pricing/     ← pure pricing types and Duration helpers
  app/                ← application services (pricing, reporting, health)
  adapters/           ← concrete implementations wired at startup
    pricing/          ← AWS, Azure, GCP, static providers
    store/            ← ring buffer (default) + Postgres (opt-in)
    anomaly/          ← RabbitMQ publisher (opt-in)
    health/           ← health checker adapter
    reporting/        ← report generator adapter
pkg/
  health/             ← ClusterHealth struct, node/pod/control-plane/network checks
  cost/               ← NodeCostData, PodCostData, NamespaceCostData, cost tracker
  anomaly/            ← Z-score detector on MetricPoint time-series
  optimizer/          ← idle/overprovisioned/cleanup analysis
  reports/            ← JSON/HTML/text report generator
configs/
  pricing-config.json ← default pricing data; mount as ConfigMap in cluster
deployment/           ← ClusterRole (read-only), ServiceAccount, Deployment YAMLs
```

### Request/data flow

1. **captureSnapshot** → `pkg/health.GetClusterHealth` + `pkg/cost` → `MetricPoint`
2. MetricPoint appended to `TimeSeriesStore` (ring buffer or Postgres)
3. `anomaly.Detector.Detect` runs Z-score over the window → emits `AnomalyEvent` to publisher
4. `reportingadapter.GenerateReport` formats health + cost data → JSON/HTML/text
5. HTTP `/api/*` routes serve the latest reports on demand; `/metrics` exposes Prometheus gauges

### HTTP API (default port 8085)

| Route | Purpose |
|---|---|
| `GET /metrics` | Prometheus metrics |
| `GET /api/health` | Health report (JSON) |
| `GET /api/cost` | Cost report (JSON) |
| `GET /api/combined` | Combined report (JSON) |
| `GET /api/optimizer` | Optimization report; supports `namespace`, `node`, `pod`, `includeIdle`, `includeOverprovisioned`, `idleCpuPercent`, `overprovisionedFactor`, `view` query params |
| `GET /api/history` | Time-series snapshots; `cluster_id`, `start`, `end`, `limit` params |
| `GET /api/anomalies` | Current anomaly set |

### Pricing provider chain

`static → aws → azure → gcp → static fallback`. Provider is selected by `configs/pricing-config.json`. AWS requires IAM `pricing:GetProducts`; GCP requires `K8S_MONITOR_GCP_API_KEY`; Azure uses a public API with no credentials. Results are cached (default 6 h TTL, SHA1 key).

### Storage

- **Default:** in-memory ring buffer (`--ring-buffer-capacity`, default 1000 points). Data lost on restart.
- **Postgres:** activated by `DATABASE_URL`. Persists `MetricPoint` snapshots. Falls back to ring buffer on connection failure.

### Adding a new backend feature

1. Define the interface in `internal/ports/`.
2. Implement the adapter in `internal/adapters/`.
3. Wire it in `cmd/main.go` under the relevant startup block.
4. Keep domain logic in `internal/domain/` or `pkg/` free of infrastructure imports.
