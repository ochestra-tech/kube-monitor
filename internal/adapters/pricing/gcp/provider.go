package gcp

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

const billingServiceID = "6F81-5844-456A" // Compute Engine

// Provider fetches GCP pricing via Cloud Billing Catalog API.
type Provider struct {
	client *http.Client
	apiKey string
	debug  bool
	logger *log.Logger
}

func New(client *http.Client, apiKey string, debug bool, logger *log.Logger) *Provider {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Provider{client: client, apiKey: apiKey, debug: debug, logger: logger}
}

func (p *Provider) Name() string {
	return "gcp"
}

type skuResponse struct {
	Skus          []skuItem `json:"skus"`
	NextPageToken string    `json:"nextPageToken"`
}

type skuItem struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Category    skuCategory   `json:"category"`
	PricingInfo []pricingInfo `json:"pricingInfo"`
}

type skuCategory struct {
	ResourceFamily string `json:"resourceFamily"`
	ResourceGroup  string `json:"resourceGroup"`
}

type pricingInfo struct {
	PricingExpression pricingExpression `json:"pricingExpression"`
}

type pricingExpression struct {
	UnitPrice money `json:"unitPrice"`
}

type money struct {
	Units        int64  `json:"units"`
	Nanos        int64  `json:"nanos"`
	CurrencyCode string `json:"currencyCode"`
}

func (p *Provider) Fetch(ctx context.Context, req ports.Request) ([]domain.InstancePricing, error) {
	if len(req.InstanceTypes) == 0 {
		return nil, fmt.Errorf("gcp instance types are required")
	}
	if p.apiKey == "" {
		return nil, fmt.Errorf("gcp api key is required for Cloud Billing Catalog")
	}

	requested := make(map[string]struct{}, len(req.InstanceTypes))
	for _, t := range req.InstanceTypes {
		requested[strings.ToLower(t)] = struct{}{}
	}

	pageToken := ""
	parts := make(map[string]*instanceParts)

	for {
		endpoint := fmt.Sprintf("https://cloudbilling.googleapis.com/v1/services/%s/skus", billingServiceID)
		query := url.Values{}
		query.Set("key", p.apiKey)
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		requestURL := fmt.Sprintf("%s?%s", endpoint, query.Encode())

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
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

		var payload skuResponse
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return nil, err
		}

		for _, sku := range payload.Skus {
			if sku.Category.ResourceFamily != "Compute" {
				continue
			}
			description := strings.ToLower(sku.Description)
			for instanceType := range requested {
				if !strings.Contains(description, instanceType) {
					continue
				}

				entry := parts[instanceType]
				if entry == nil {
					entry = &instanceParts{}
					parts[instanceType] = entry
				}

				if entry.currency == "" && len(sku.PricingInfo) > 0 {
					entry.currency = sku.PricingInfo[0].PricingExpression.UnitPrice.CurrencyCode
				}

				vcpu, memGB := parseResourceCounts(sku.Description)
				if entry.vcpu == 0 && vcpu > 0 {
					entry.vcpu = vcpu
				}
				if entry.memGB == 0 && memGB > 0 {
					entry.memGB = memGB
				}

				price := extractPrice(sku.PricingInfo)
				if price <= 0 {
					continue
				}

				group := strings.ToLower(sku.Category.ResourceGroup)
				switch {
				case strings.Contains(group, "cpu"):
					entry.cpuUnit = price
				case strings.Contains(group, "ram"):
					entry.memUnit = price
				default:
					if entry.total == 0 {
						entry.total = price
					}
				}
			}
		}

		if payload.NextPageToken == "" {
			break
		}
		pageToken = payload.NextPageToken
	}

	results := make(map[string]domain.InstancePricing)
	for instanceType := range requested {
		entry := parts[instanceType]
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
		p.debugf("[pricing][gcp] %s vcpu=%.2f mem=%.2f total=%.6f cpuUnit=%.6f memUnit=%.6f",
			instanceType, entry.vcpu, entry.memGB, pricing.TotalPerHour, pricing.CPU, pricing.Memory)

		results[instanceType] = domain.InstancePricing{
			InstanceType: instanceType,
			Region:       req.Region,
			Currency:     entry.currency,
			Source:       p.Name(),
			Pricing:      pricing,
		}
	}

	return mapToList(results), nil
}

func extractPrice(pricing []pricingInfo) float64 {
	if len(pricing) == 0 {
		return 0
	}
	price := pricing[0].PricingExpression.UnitPrice
	return float64(price.Units) + float64(price.Nanos)/1e9
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
	vcpu     float64
	memGB    float64
	cpuUnit  float64
	memUnit  float64
	total    float64
	currency string
}
