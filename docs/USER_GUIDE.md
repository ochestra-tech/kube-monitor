# K8s Monitor — User Guide

## Overview
K8s Monitor provides Kubernetes health checks, cost reporting, optimization hints, and Prometheus metrics. It supports cloud pricing from AWS, Azure, and GCP, or static pricing from local configuration.

## Quick Start
### Build
```bash
go build -o k8s-monitor ./cmd
```

### Run a one‑shot combined report
```bash
./k8s-monitor --one-shot --type combined --format text
```

### Continuous monitoring with metrics
```bash
./k8s-monitor --interval 5m --metrics-port 8080
```

### Enable detailed metrics (top 10 namespaces)
```bash
./k8s-monitor --enable-detailed-metrics --metrics-top-namespaces 10
```

## CLI Reference
| Flag | Description | Default |
| --- | --- | --- |
| `--kubeconfig` | Path to kubeconfig file | `~/.kube/config` |
| `--pricing-config` | Path to pricing configuration | `configs/pricing-config.json` |
| `--type` | Report type: `health`, `cost`, `combined` | `combined` |
| `--format` | Output format: `text`, `json`, `html` | `text` |
| `--output` | Output file path (empty for stdout) | `` |
| `--interval` | Check interval for continuous monitoring | `60s` |
| `--metrics-port` | Prometheus metrics port | `8080` |
| `--metrics-read-header-timeout` | Metrics server read header timeout | `5s` |
| `--request-timeout` | Per-report timeout | `30s` |
| `--shutdown-timeout` | Graceful shutdown timeout | `10s` |
| `--enable-detailed-metrics` | Enable namespace/phase metrics | `false` |
| `--metrics-top-namespaces` | Max namespaces in metrics | `10` |
| `--detailed-metrics-interval` | Detailed metrics refresh interval | `5m` |
| `--pricing-debug` | Enable pricing debug logging | `false` |
| `--pricing-debug-log` | Pricing debug log file | `pricing-debug.log` |
| `--one-shot` | Run once and exit | `false` |

## Pricing Configuration
The pricing configuration lives at `configs/pricing-config.json` by default.

### Source Selection
Set `source` to:
- `static` — uses only the local `instanceTypes` mapping
- `aws` — use AWS Pricing API
- `azure` — use Azure Retail Prices API
- `gcp` — use GCP Cloud Billing Catalog API
- `auto` — try AWS → Azure → GCP → static

### Providers
```json
{
  "source": "auto",
  "cacheTtl": "6h",
  "defaults": {
    "cpu": 0.03,
    "memory": 0.004,
    "storage": 0.00012,
    "network": 0.08
  },
  "instanceTypes": {
    "m5.large": { "cpu": 0.032, "memory": 0.0045, "storage": 0.00015, "network": 0.09 }
  },
  "regionMultipliers": {
    "us-east-1": 1.0,
    "us-west-2": 1.05
  },
  "providers": {
    "aws": {
      "region": "us-east-1",
      "currency": "USD",
      "operatingSystem": "Linux",
      "tenancy": "Shared"
    },
    "azure": {
      "region": "eastus",
      "currency": "USD"
    },
    "gcp": {
      "region": "us-central1",
      "currency": "USD",
      "apiKey": ""
    }
  },
  "retry": {
    "attempts": 3,
    "backoff": "1s"
  }
}
```

### GCP API Key
Set `K8S_MONITOR_GCP_API_KEY` as an environment variable or fill `providers.gcp.apiKey`.

## Reports
### Health Report
```bash
./k8s-monitor --type health --format text
```

### Cost Report
```bash
./k8s-monitor --type cost --format json --output cost-report.json
```

### Combined Report
```bash
./k8s-monitor --type combined --format html --output cluster-report.html
```

## Prometheus Metrics
### Core metrics
- `k8s_monitor_report_duration_seconds{report_type,status}`
- `k8s_monitor_report_total{report_type,status}`
- `k8s_monitor_report_last_success_timestamp_seconds{report_type}`

### Detailed metrics (flag‑gated)
- `k8s_monitor_pods_phase_total{phase}`
- `k8s_monitor_namespace_pods_phase_total{namespace,phase}`
- `k8s_monitor_namespace_pods_total{namespace}`

## Kubernetes Deployment
Use the manifests under `deployment/`:
- `deployment.yaml`
- `serviceaccount.yaml`
- `clusterrole.yaml`
- `clusterrolebinding.yaml`

Example:
```bash
kubectl apply -f deployment/serviceaccount.yaml
kubectl apply -f deployment/clusterrole.yaml
kubectl apply -f deployment/clusterrolebinding.yaml
kubectl apply -f deployment/deployment.yaml
```

## Troubleshooting
- **No pricing data**: ensure `source` is correct and provider settings are valid.
- **GCP failures**: verify `K8S_MONITOR_GCP_API_KEY` or `providers.gcp.apiKey`.
- **Metrics not exposed**: check `--metrics-port` and service/network policies.
- **Slow report**: increase `--request-timeout` or reduce report type complexity.

## Security Notes
- Keep pricing API keys in environment variables or secret mounts.
- Detailed metrics are bounded by top‑N namespaces to avoid cardinality blowups.
