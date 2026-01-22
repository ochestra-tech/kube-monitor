package optimizer

import (
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/metrics/pkg/client/clientset/versioned"
)

type ReportView string

const (
	ViewFull    ReportView = "full"
	ViewSummary ReportView = "summary"
	ViewDetails ReportView = "details"
)

type Options struct {
	// Filters
	Namespace string
	Node      string
	Pod       string

	// Toggles
	IncludeIdle            bool
	IncludeOverprovisioned bool
	IncludeCleanup         bool
	DryRunCleanup          bool
	IncludeNetwork         bool
	IncludeStorage         bool

	// Thresholds
	// Idle thresholds are interpreted as % of requests (0-100).
	IdleCPUPercent    float64
	IdleMemoryPercent float64

	// OverprovisionedFactor flags resources where request > usage * factor.
	OverprovisionedFactor float64

	// HeadroomFactor suggests new request ~= usage * headroomFactor.
	HeadroomFactor float64

	// NetworkIdleBytesPerSec flags pods with total rx+tx <= threshold.
	NetworkIdleBytesPerSec float64

	// StorageLowUtilPercent flags PVCs with used/capacity <= threshold (0-100).
	StorageLowUtilPercent float64

	View ReportView
}

type OptimizationReport struct {
	GeneratedAt time.Time               `json:"generated_at"`
	Options     Options                 `json:"options"`
	Summary     Summary                 `json:"summary"`
	Details     []Detail                `json:"details,omitempty"`
	Cleanup     []CleanupRecommendation `json:"cleanup,omitempty"`
}

type Summary struct {
	PotentialMonthlySavings float64 `json:"potential_monthly_savings"`
	IdleResources           Counts  `json:"idle_resources"`
	Overprovisioned         Counts  `json:"overprovisioned"`
	WastedCPUCoreHours      float64 `json:"wasted_cpu_core_hours"`
	WastedMemoryGBHours     float64 `json:"wasted_memory_gb_hours"`
}

type Counts struct {
	Nodes      int `json:"nodes"`
	Namespaces int `json:"namespaces"`
	Pods       int `json:"pods"`
	Containers int `json:"containers"`
}

type Detail struct {
	Category  string `json:"category"` // idle | overprovisioned
	Level     string `json:"level"`    // node | namespace | pod | container | pvc
	Node      string `json:"node,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Pod       string `json:"pod,omitempty"`
	Container string `json:"container,omitempty"`
	PVC       string `json:"pvc,omitempty"`

	CPUUsageMillicores   int64 `json:"cpu_usage_millicores,omitempty"`
	CPURequestMillicores int64 `json:"cpu_request_millicores,omitempty"`
	CPULimitMillicores   int64 `json:"cpu_limit_millicores,omitempty"`
	MemoryUsageBytes     int64 `json:"memory_usage_bytes,omitempty"`
	MemoryRequestBytes   int64 `json:"memory_request_bytes,omitempty"`
	MemoryLimitBytes     int64 `json:"memory_limit_bytes,omitempty"`

	WastedCPUMillicores    int64   `json:"wasted_cpu_millicores,omitempty"`
	WastedMemoryBytes      int64   `json:"wasted_memory_bytes,omitempty"`
	PotentialMonthlySaving float64 `json:"potential_monthly_saving,omitempty"`

	NetworkReceiveBytesPerSec  float64 `json:"network_receive_bytes_per_sec,omitempty"`
	NetworkTransmitBytesPerSec float64 `json:"network_transmit_bytes_per_sec,omitempty"`

	PVCUsedBytes          int64   `json:"pvc_used_bytes,omitempty"`
	PVCCapacityBytes      int64   `json:"pvc_capacity_bytes,omitempty"`
	PVCUtilizationPercent float64 `json:"pvc_utilization_percent,omitempty"`
	PVCWastedBytes        int64   `json:"pvc_wasted_bytes,omitempty"`

	Recommendation string `json:"recommendation,omitempty"`
}

type ResourceOptimizer struct {
	clientset     *kubernetes.Clientset
	metricsClient *versioned.Clientset
	promClient    *PrometheusClient
}

func NewResourceOptimizer(clientset *kubernetes.Clientset, metricsClient *versioned.Clientset) *ResourceOptimizer {
	return &ResourceOptimizer{
		clientset:     clientset,
		metricsClient: metricsClient,
	}
}

func NewResourceOptimizerWithPrometheus(clientset *kubernetes.Clientset, metricsClient *versioned.Clientset, prometheusURL string) *ResourceOptimizer {
	opt := NewResourceOptimizer(clientset, metricsClient)
	if strings.TrimSpace(prometheusURL) != "" {
		opt.promClient = NewPrometheusClient(prometheusURL)
	}
	return opt
}

func InitInClusterClients() (*kubernetes.Clientset, *versioned.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}

	metricsClient, err := versioned.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}

	return clientset, metricsClient, nil
}

type CleanupRecommendation struct {
	ResourceType string `json:"resource_type"`
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Reason       string `json:"reason"`
	AgeSeconds   int64  `json:"age,omitempty"`
}
