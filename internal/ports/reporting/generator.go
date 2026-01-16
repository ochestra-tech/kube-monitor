package reporting

import (
	"context"

	"github.com/ochestra-tech/k8s-monitor/pkg/cost"
)

// Generator builds reports in different formats.
type Generator interface {
	GenerateHealthReport(ctx context.Context) error
	GenerateCostReport(ctx context.Context, pricing map[string]cost.ResourcePricing) error
	GenerateCombinedReport(ctx context.Context, pricing map[string]cost.ResourcePricing) error
}
