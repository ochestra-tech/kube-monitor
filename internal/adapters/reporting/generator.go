package reporting

import (
	"context"
	"io"

	"github.com/ochestra-tech/k8s-monitor/pkg/cost"
	"github.com/ochestra-tech/k8s-monitor/pkg/reports"
	"k8s.io/client-go/kubernetes"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

// GeneratorAdapter bridges the reports package.
type GeneratorAdapter struct {
	generator *reports.ReportGenerator
}

func NewGenerator(clientset *kubernetes.Clientset, metricsClient *metricsv.Clientset, format reports.ReportFormat, writer io.Writer) *GeneratorAdapter {
	return &GeneratorAdapter{generator: reports.NewReportGenerator(clientset, metricsClient, format, writer)}
}

func (g *GeneratorAdapter) GenerateHealthReport(ctx context.Context) error {
	return g.generator.GenerateHealthReport(ctx)
}

func (g *GeneratorAdapter) GenerateCostReport(ctx context.Context, pricing map[string]cost.ResourcePricing) error {
	return g.generator.GenerateCostReport(ctx, pricing)
}

func (g *GeneratorAdapter) GenerateCombinedReport(ctx context.Context, pricing map[string]cost.ResourcePricing) error {
	return g.generator.GenerateCombinedReport(ctx, pricing)
}
