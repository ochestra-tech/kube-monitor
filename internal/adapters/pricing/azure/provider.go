package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	domain "github.com/ochestra-tech/k8s-monitor/internal/domain/pricing"
	ports "github.com/ochestra-tech/k8s-monitor/internal/ports/pricing"
)

const retailPricesEndpoint = "https://prices.azure.com/api/retail/prices"

// Provider fetches Azure retail prices.
type Provider struct {
	client *http.Client
	debug  bool
	logger *log.Logger
}

func New(client *http.Client, debug bool, logger *log.Logger) *Provider {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Provider{client: client, debug: debug, logger: logger}
}

func (p *Provider) Name() string {
	return "azure"
}

type retailResponse struct {
	Items        []retailItem `json:"Items"`
	NextPageLink string       `json:"NextPageLink"`
}

type retailItem struct {
	ArmSkuName   string  `json:"armSkuName"`
	ArmRegion    string  `json:"armRegionName"`
	CurrencyCode string  `json:"currencyCode"`
	UnitPrice    float64 `json:"unitPrice"`
	PriceType    string  `json:"priceType"`
	ServiceName  string  `json:"serviceName"`
	ProductName  string  `json:"productName"`
	SkuName      string  `json:"skuName"`
	MeterName    string  `json:"meterName"`
}

func (p *Provider) Fetch(ctx context.Context, req ports.Request) ([]domain.InstancePricing, error) {
	if req.Region == "" {
		return nil, fmt.Errorf("azure region is required")
	}

	requested := make(map[string]struct{}, len(req.InstanceTypes))
	for _, t := range req.InstanceTypes {
		requested[strings.ToLower(t)] = struct{}{}
	}

	filter := fmt.Sprintf("serviceName eq 'Virtual Machines' and armRegionName eq '%s' and priceType eq 'Consumption'", req.Region)
	query := url.Values{}
	query.Set("$filter", filter)

	pageURL := fmt.Sprintf("%s?%s", retailPricesEndpoint, query.Encode())
	parts := make(map[string]*instanceParts)

	for pageURL != "" {
		reqURL := pageURL
		pageURL = ""

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}

		resp, err := p.client.Do(httpReq)
		if err != nil {
			return nil, err
		}
		if resp.Body != nil {
			defer resp.Body.Close()
		}

		var payload retailResponse
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return nil, err
		}

		for _, item := range payload.Items {
			sku := strings.ToLower(item.ArmSkuName)
			if len(requested) > 0 {
				if _, ok := requested[sku]; !ok {
					continue
				}
			}

			entry := parts[sku]
			if entry == nil {
				entry = &instanceParts{}
				parts[sku] = entry
			}
			if entry.instanceType == "" {
				entry.instanceType = item.ArmSkuName
			}
			if entry.region == "" {
				entry.region = item.ArmRegion
			}

			if entry.currency == "" {
				entry.currency = item.CurrencyCode
			}

			vcpu, memGB := parseResourceCounts(item.ProductName, item.SkuName, item.MeterName)
			if entry.vcpu == 0 && vcpu > 0 {
				entry.vcpu = vcpu
			}
			if entry.memGB == 0 && memGB > 0 {
				entry.memGB = memGB
			}

			meter := strings.ToLower(item.MeterName)
			product := strings.ToLower(item.ProductName)
			skuName := strings.ToLower(item.SkuName)
			if strings.Contains(meter, "vcore") || strings.Contains(meter, "vcpu") || strings.Contains(product, "vcore") || strings.Contains(product, "vcpu") {
				entry.cpuUnit = item.UnitPrice
			} else if strings.Contains(meter, "ram") || strings.Contains(meter, "memory") || strings.Contains(skuName, "ram") {
				entry.memUnit = item.UnitPrice
			} else if entry.total == 0 {
				entry.total = item.UnitPrice
			}
		}

		if payload.NextPageLink != "" {
			pageURL = payload.NextPageLink
		}
	}

	results := make(map[string]domain.InstancePricing)
	for sku := range requested {
		entry := parts[sku]
		if entry == nil {
			continue
		}

		pricing := domain.ResourcePricing{}
		if entry.cpuUnit > 0 && entry.memUnit > 0 && entry.vcpu > 0 && entry.memGB > 0 {
			pricing.CPU = entry.cpuUnit
			pricing.Memory = entry.memUnit
			pricing.TotalPerHour = entry.cpuUnit*entry.vcpu + entry.memUnit*entry.memGB
		} else if entry.total > 0 {
			pricing.TotalPerHour = entry.total
			if entry.vcpu > 0 && entry.memGB > 0 {
				cpuCost, memCost := splitByResources(entry.total, entry.vcpu, entry.memGB)
				pricing.CPU = cpuCost / entry.vcpu
				pricing.Memory = memCost / entry.memGB
			}
		}
		p.debugf("[pricing][azure] %s vcpu=%.2f mem=%.2f total=%.6f cpuUnit=%.6f memUnit=%.6f",
			entry.instanceType, entry.vcpu, entry.memGB, pricing.TotalPerHour, pricing.CPU, pricing.Memory)

		results[sku] = domain.InstancePricing{
			InstanceType: entry.instanceType,
			Region:       entry.region,
			Currency:     entry.currency,
			Source:       p.Name(),
			Pricing:      pricing,
		}
	}

	return mapToList(results), nil
}

func mapToList(items map[string]domain.InstancePricing) []domain.InstancePricing {
	results := make([]domain.InstancePricing, 0, len(items))
	for _, item := range items {
		results = append(results, item)
	}
	return results
}

func parseResourceCounts(parts ...string) (float64, float64) {
	text := strings.ToLower(strings.Join(parts, " "))
	vcpu := matchNumber(text, `(?i)(\d+(?:\.\d+)?)\s*(vcpus?|cores?)`)
	mem := matchNumber(text, `(?i)(\d+(?:\.\d+)?)\s*(gib|gb)`)
	return vcpu, mem
}

func matchNumber(text, pattern string) float64 {
	re := regexp.MustCompile(pattern)
	matches := re.FindStringSubmatch(text)
	if len(matches) < 2 {
		return 0
	}
	value, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0
	}
	return value
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

type instanceParts struct {
	instanceType string
	region       string
	vcpu         float64
	memGB        float64
	cpuUnit      float64
	memUnit      float64
	total        float64
	currency     string
}
