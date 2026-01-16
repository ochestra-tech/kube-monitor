package pricing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	domain "github.com/ochestra-tech/k8s-monitor/internal/domain/pricing"
)

const defaultSource = "static"

// LoadConfig loads pricing configuration and writes defaults if missing.
func LoadConfig(path string) (domain.Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		cfg := defaultConfig()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return domain.Config{}, err
		}
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return domain.Config{}, err
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return domain.Config{}, err
		}
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Config{}, err
	}

	var cfg domain.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return domain.Config{}, err
	}

	if cfg.Source == "" {
		cfg.Source = defaultSource
	}
	if cfg.CacheTTL.Duration == 0 {
		cfg.CacheTTL.Duration = 6 * time.Hour
	}
	if cfg.Retry.Attempts == 0 {
		cfg.Retry.Attempts = 3
	}
	if cfg.Retry.Backoff.Duration == 0 {
		cfg.Retry.Backoff.Duration = 1 * time.Second
	}
	return cfg, nil
}

func defaultConfig() domain.Config {
	return domain.Config{
		Source: "static",
		Defaults: domain.ResourcePricing{
			CPU:     0.03,
			Memory:  0.004,
			Storage: 0.00012,
			Network: 0.08,
			GPUPricing: map[string]float64{
				"nvidia-tesla-v100": 1.2,
				"nvidia-tesla-k80":  0.6,
			},
		},
		InstanceTypes: map[string]domain.ResourcePricing{
			"m5.large": {
				CPU:     0.032,
				Memory:  0.0045,
				Storage: 0.00015,
				Network: 0.09,
			},
			"c5.large": {
				CPU:     0.035,
				Memory:  0.0035,
				Storage: 0.00018,
				Network: 0.095,
			},
		},
		Regions: map[string]float64{
			"us-east-1":      1.0,
			"us-west-2":      1.05,
			"eu-west-1":      1.1,
			"ap-southeast-1": 1.15,
		},
		Providers: domain.ProvidersConfig{
			AWS: domain.AWSConfig{
				Region:          "us-east-1",
				Currency:        "USD",
				OperatingSystem: "Linux",
				Tenancy:         "Shared",
			},
			Azure: domain.AzureConfig{
				Region:   "eastus",
				Currency: "USD",
			},
			GCP: domain.GCPConfig{
				Region:   "us-central1",
				Currency: "USD",
				APIKey:   "",
			},
		},
		CacheTTL: domain.Duration{Duration: 6 * time.Hour},
		Retry: domain.RetryConfig{
			Attempts: 3,
			Backoff:  domain.Duration{Duration: 1 * time.Second},
		},
	}
}
