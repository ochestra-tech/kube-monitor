package health

import (
	"context"

	"github.com/ochestra-tech/k8s-monitor/pkg/health"
	"k8s.io/client-go/kubernetes"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

// CheckerAdapter bridges the health package into a port.
type CheckerAdapter struct {
	clientset     *kubernetes.Clientset
	metricsClient *metricsv.Clientset
}

func NewChecker(clientset *kubernetes.Clientset, metricsClient *metricsv.Clientset) *CheckerAdapter {
	return &CheckerAdapter{clientset: clientset, metricsClient: metricsClient}
}

func (c *CheckerAdapter) Check(ctx context.Context) (*health.ClusterHealth, error) {
	return health.GetClusterHealth(ctx, c.clientset, c.metricsClient)
}
