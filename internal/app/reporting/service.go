package reporting

import (
	"context"
	"fmt"

	ports "github.com/ochestra-tech/k8s-monitor/internal/ports/reporting"
	"github.com/ochestra-tech/k8s-monitor/pkg/cost"
)

// Service orchestrates report generation.
type Service struct {
	generator ports.Generator
}

func NewService(generator ports.Generator) *Service {
	return &Service{generator: generator}
}

func (s *Service) Generate(ctx context.Context, reportType string, pricing map[string]cost.ResourcePricing) error {
	switch reportType {
	case "health":
		return s.generator.GenerateHealthReport(ctx)
	case "cost":
		return s.generator.GenerateCostReport(ctx, pricing)
	case "combined":
		return s.generator.GenerateCombinedReport(ctx, pricing)
	default:
		return fmt.Errorf("unknown report type: %s", reportType)
	}
}
