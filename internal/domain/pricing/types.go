package pricing

// ResourcePricing describes hourly pricing for compute resources.
type ResourcePricing struct {
	CPU          float64            `json:"cpu"`
	Memory       float64            `json:"memory"`
	Storage      float64            `json:"storage"`
	Network      float64            `json:"network"`
	TotalPerHour float64            `json:"totalPerHour"`
	GPUPricing   map[string]float64 `json:"gpuPricing"`
}

// InstancePricing represents pricing for a specific VM instance type.
type InstancePricing struct {
	InstanceType string          `json:"instanceType"`
	Region       string          `json:"region"`
	Currency     string          `json:"currency"`
	Source       string          `json:"source"`
	Pricing      ResourcePricing `json:"pricing"`
}

// Config describes how pricing should be resolved.
type Config struct {
	Source        string                     `json:"source"`
	Defaults      ResourcePricing            `json:"defaults"`
	InstanceTypes map[string]ResourcePricing `json:"instanceTypes"`
	Regions       map[string]float64         `json:"regionMultipliers"`
	Providers     ProvidersConfig            `json:"providers"`
	CacheTTL      Duration                   `json:"cacheTtl"`
	Retry         RetryConfig                `json:"retry"`
}

// RetryConfig configures provider retries.
type RetryConfig struct {
	Attempts int      `json:"attempts"`
	Backoff  Duration `json:"backoff"`
}

// ProvidersConfig contains provider-specific configuration.
type ProvidersConfig struct {
	AWS   AWSConfig   `json:"aws"`
	Azure AzureConfig `json:"azure"`
	GCP   GCPConfig   `json:"gcp"`
}

// AWSConfig describes AWS pricing API configuration.
type AWSConfig struct {
	Region          string `json:"region"`
	Currency        string `json:"currency"`
	OperatingSystem string `json:"operatingSystem"`
	Tenancy         string `json:"tenancy"`
}

// AzureConfig describes Azure retail prices configuration.
type AzureConfig struct {
	Region   string `json:"region"`
	Currency string `json:"currency"`
}

// GCPConfig describes GCP Cloud Billing configuration.
type GCPConfig struct {
	Region   string `json:"region"`
	Currency string `json:"currency"`
	APIKey   string `json:"apiKey"`
}
