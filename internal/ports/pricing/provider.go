package pricing

import (
	"context"

	"github.com/ochestra-tech/k8s-monitor/internal/domain/pricing"
)

// Request specifies pricing lookup parameters.
type Request struct {
	Region          string
	Currency        string
	OperatingSystem string
	Tenancy         string
	InstanceTypes   []string
}

// Provider fetches pricing data from a source.
type Provider interface {
	Name() string
	Fetch(ctx context.Context, req Request) ([]pricing.InstancePricing, error)
}
