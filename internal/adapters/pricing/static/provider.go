package static

import (
	"context"

	domain "github.com/ochestra-tech/k8s-monitor/internal/domain/pricing"
	ports "github.com/ochestra-tech/k8s-monitor/internal/ports/pricing"
)

// Provider returns pricing from static configuration.
type Provider struct {
	pricing  map[string]domain.ResourcePricing
	region   string
	currency string
}

func New(pricing map[string]domain.ResourcePricing, region, currency string) *Provider {
	return &Provider{pricing: pricing, region: region, currency: currency}
}

func (p *Provider) Name() string {
	return "static"
}

func (p *Provider) Fetch(ctx context.Context, req ports.Request) ([]domain.InstancePricing, error) {
	_ = ctx
	instanceTypes := req.InstanceTypes
	if len(instanceTypes) == 0 {
		for name := range p.pricing {
			instanceTypes = append(instanceTypes, name)
		}
	}

	results := make([]domain.InstancePricing, 0, len(instanceTypes))
	for _, t := range instanceTypes {
		if price, ok := p.pricing[t]; ok {
			results = append(results, domain.InstancePricing{
				InstanceType: t,
				Region:       req.Region,
				Currency:     req.Currency,
				Source:       p.Name(),
				Pricing:      price,
			})
		}
	}

	return results, nil
}
