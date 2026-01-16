package health

import (
	"context"

	"github.com/ochestra-tech/k8s-monitor/pkg/health"
)

// Checker defines health checks for a cluster.
type Checker interface {
	Check(ctx context.Context) (*health.ClusterHealth, error)
}
