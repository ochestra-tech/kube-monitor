package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	awstypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	domain "github.com/ochestra-tech/k8s-monitor/internal/domain/pricing"
	ports "github.com/ochestra-tech/k8s-monitor/internal/ports/pricing"
)

// Provider fetches AWS On-Demand pricing via the Pricing API.
type Provider struct {
	client *pricing.Client
	debug  bool
	logger *log.Logger
}

func New(ctx context.Context, debug bool, logger *log.Logger) (*Provider, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		return nil, err
	}
	return &Provider{client: pricing.NewFromConfig(cfg), debug: debug, logger: logger}, nil
}

func (p *Provider) Name() string {
	return "aws"
}

func (p *Provider) Fetch(ctx context.Context, req ports.Request) ([]domain.InstancePricing, error) {
	if req.Region == "" {
		return nil, fmt.Errorf("aws region is required")
	}
	if len(req.InstanceTypes) == 0 {
		return nil, fmt.Errorf("aws instance types are required")
	}

	operatingSystem := req.OperatingSystem
	if operatingSystem == "" {
		operatingSystem = "Linux"
	}
	tenancy := req.Tenancy
	if tenancy == "" {
		tenancy = "Shared"
	}

	results := make(map[string]domain.InstancePricing)
	for _, instanceType := range req.InstanceTypes {
		filters := []awstypes.Filter{
			{Type: awstypes.FilterTypeTermMatch, Field: awsString("instanceType"), Value: awsString(instanceType)},
			{Type: awstypes.FilterTypeTermMatch, Field: awsString("location"), Value: awsString(regionName(req.Region))},
			{Type: awstypes.FilterTypeTermMatch, Field: awsString("operatingSystem"), Value: awsString(operatingSystem)},
			{Type: awstypes.FilterTypeTermMatch, Field: awsString("tenancy"), Value: awsString(tenancy)},
			{Type: awstypes.FilterTypeTermMatch, Field: awsString("preInstalledSw"), Value: awsString("NA")},
			{Type: awstypes.FilterTypeTermMatch, Field: awsString("capacitystatus"), Value: awsString("Used")},
		}

		input := &pricing.GetProductsInput{
			ServiceCode: awsString("AmazonEC2"),
			Filters:     filters,
		}

		out, err := p.client.GetProducts(ctx, input)
		if err != nil {
			return nil, err
		}
		if len(out.PriceList) == 0 {
			continue
		}

		price, currency, vcpu, memGB := extractOnDemandPrice(json.RawMessage(out.PriceList[0]))
		if price <= 0 {
			continue
		}

		pricing := domain.ResourcePricing{TotalPerHour: price}
		if vcpu > 0 && memGB > 0 {
			cpuCost, memCost := splitByResources(price, vcpu, memGB)
			pricing.CPU = cpuCost / vcpu
			pricing.Memory = memCost / memGB
		}
		p.debugf("[pricing][aws] %s vcpu=%.2f mem=%.2f total=%.6f cpuUnit=%.6f memUnit=%.6f",
			instanceType, vcpu, memGB, pricing.TotalPerHour, pricing.CPU, pricing.Memory)

		results[strings.ToLower(instanceType)] = domain.InstancePricing{
			InstanceType: instanceType,
			Region:       req.Region,
			Currency:     currency,
			Source:       p.Name(),
			Pricing:      pricing,
		}
	}

	return mapToList(results), nil
}

func extractOnDemandPrice(raw json.RawMessage) (float64, string, float64, float64) {
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, "", 0, 0
	}

	vcpu, memGB := extractResources(payload)

	terms, ok := payload["terms"].(map[string]interface{})
	if !ok {
		return 0, "", vcpu, memGB
	}
	od, ok := terms["OnDemand"].(map[string]interface{})
	if !ok {
		return 0, "", vcpu, memGB
	}

	for _, term := range od {
		termMap, ok := term.(map[string]interface{})
		if !ok {
			continue
		}
		priceDimensions, ok := termMap["priceDimensions"].(map[string]interface{})
		if !ok {
			continue
		}
		for _, dim := range priceDimensions {
			dimMap, ok := dim.(map[string]interface{})
			if !ok {
				continue
			}
			pricePerUnit, ok := dimMap["pricePerUnit"].(map[string]interface{})
			if !ok {
				continue
			}
			for currency, value := range pricePerUnit {
				valueStr, ok := value.(string)
				if !ok {
					continue
				}
				price, err := strconv.ParseFloat(valueStr, 64)
				if err != nil {
					continue
				}
				return price, currency, vcpu, memGB
			}
		}
	}

	return 0, "", vcpu, memGB
}

func regionName(code string) string {
	// AWS Pricing API uses human-readable region names.
	mapping := map[string]string{
		"us-east-1":      "US East (N. Virginia)",
		"us-east-2":      "US East (Ohio)",
		"us-west-1":      "US West (N. California)",
		"us-west-2":      "US West (Oregon)",
		"eu-west-1":      "EU (Ireland)",
		"eu-west-2":      "EU (London)",
		"eu-central-1":   "EU (Frankfurt)",
		"ap-southeast-1": "Asia Pacific (Singapore)",
		"ap-southeast-2": "Asia Pacific (Sydney)",
	}
	if name, ok := mapping[code]; ok {
		return name
	}
	return code
}

func awsString(value string) *string {
	return &value
}

func (p *Provider) debugf(format string, args ...interface{}) {
	if !p.debug {
		return
	}
	if p.logger != nil {
		p.logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func extractResources(payload map[string]interface{}) (float64, float64) {
	product, ok := payload["product"].(map[string]interface{})
	if !ok {
		return 0, 0
	}
	attrs, ok := product["attributes"].(map[string]interface{})
	if !ok {
		return 0, 0
	}
	vcpu := parseFloatAttr(attrs, "vcpu")
	memGB := parseMemoryAttr(attrs, "memory")
	return vcpu, memGB
}

func parseFloatAttr(attrs map[string]interface{}, key string) float64 {
	value, ok := attrs[key].(string)
	if !ok {
		return 0
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return 0
	}
	parsed, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	return parsed
}

func parseMemoryAttr(attrs map[string]interface{}, key string) float64 {
	value, ok := attrs[key].(string)
	if !ok {
		return 0
	}
	value = strings.ReplaceAll(value, "GiB", "")
	value = strings.ReplaceAll(value, "GB", "")
	value = strings.TrimSpace(value)
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return 0
	}
	parsed, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	return parsed
}

func splitByResources(total, vcpu, memGB float64) (float64, float64) {
	if total <= 0 || vcpu <= 0 || memGB <= 0 {
		return 0, 0
	}
	denom := vcpu + memGB
	if denom == 0 {
		return 0, 0
	}
	cpuCost := total * (vcpu / denom)
	memCost := total * (memGB / denom)
	return cpuCost, memCost
}

func mapToList(items map[string]domain.InstancePricing) []domain.InstancePricing {
	results := make([]domain.InstancePricing, 0, len(items))
	for _, item := range items {
		results = append(results, item)
	}
	return results
}
