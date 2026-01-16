# K8s Monitor — SRE / Cluster Admin Guide

## Purpose
This guide focuses on running K8s Monitor reliably in production clusters, with an emphasis on security, performance, and operational safety.

## Deployment Checklist
- [ ] ServiceAccount and RBAC applied
- [ ] Metrics server running in the cluster (optional but recommended)
- [ ] Pricing configuration mounted or available in container image
- [ ] Network policies allow access to Kubernetes API and metrics server
- [ ] Monitoring stack (Prometheus) configured to scrape `/metrics`

## RBAC Scope
K8s Monitor only needs **read-only** access unless you explicitly run cleanup features. Apply:
- [deployment/serviceaccount.yaml](deployment/serviceaccount.yaml)
- [deployment/clusterrole.yaml](deployment/clusterrole.yaml)
- [deployment/clusterrolebinding.yaml](deployment/clusterrolebinding.yaml)

## Resource Requests
Suggested baseline:
- CPU: 100m–250m
- Memory: 128Mi–512Mi

Tune upward for large clusters.

## Metrics and Cardinality Control
By default, only low-cardinality metrics are exposed. To enable namespace/phase metrics:
```bash
./k8s-monitor --enable-detailed-metrics --metrics-top-namespaces 10
```
Safeguards:
- top‑N namespaces only
- `other` bucket for all remaining namespaces

Set a separate collection cadence:
```bash
./k8s-monitor --detailed-metrics-interval 10m
```

## Pricing Providers
### AWS Pricing API
- Uses AWS Pricing API (region: `us-east-1` API endpoint)
- Requires AWS credentials with `pricing:GetProducts`

### Azure Retail Prices
- Uses public Retail Prices endpoint
- No credentials required

### GCP Cloud Billing Catalog
- Requires API key
- Set via `K8S_MONITOR_GCP_API_KEY` or `providers.gcp.apiKey`

## Debugging Pricing
Enable detailed provider logs:
```bash
./k8s-monitor --pricing-debug --pricing-debug-log /var/log/k8s-monitor/pricing-debug.log
```
Logs include computed vCPU/GB splits and total hourly costs.

## Recommended Liveness/Readiness
Add probes (example):
```yaml
livenessProbe:
  httpGet:
    path: /metrics
    port: 8080
  initialDelaySeconds: 10
  periodSeconds: 30
readinessProbe:
  httpGet:
    path: /metrics
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 10
```

## Scaling
K8s Monitor is typically run as a single replica. Avoid multiple replicas unless you intentionally want duplicate reports and metrics.

## Logging
- Application logs go to stdout
- Pricing debug logs go to file when enabled

## Security Notes
- Store pricing API keys in Kubernetes Secrets
- Use a dedicated ServiceAccount with least privileges
- Avoid enabling cleanup logic in production

## Troubleshooting
- **API rate limits**: increase `--interval` and/or `--detailed-metrics-interval`
- **High CPU usage**: reduce detailed metrics and report frequency
- **Pricing missing**: verify provider config and credentials

## Example Kubernetes Apply
```bash
kubectl apply -f deployment/serviceaccount.yaml
kubectl apply -f deployment/clusterrole.yaml
kubectl apply -f deployment/clusterrolebinding.yaml
kubectl apply -f deployment/deployment.yaml
```
