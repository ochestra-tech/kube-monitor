package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"k8s.io/metrics/pkg/client/clientset/versioned"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/ochestra-tech/k8s-monitor/pkg/cost"
	"github.com/ochestra-tech/k8s-monitor/pkg/health"
	"github.com/ochestra-tech/k8s-monitor/pkg/reports"
)

// Config holds the application configuration
type Config struct {
	KubeConfigPath    string
	PricingConfigPath string
	ReportFormat      reports.ReportFormat
	ReportPath        string
	CheckInterval     time.Duration
	MetricsPort       int
	OneShot           bool
	ReportType        string // "health", "cost", "combined"
}

// PricingConfig holds the pricing configuration structure
type PricingConfig struct {
	Defaults      cost.ResourcePricing            `json:"defaults"`
	InstanceTypes map[string]cost.ResourcePricing `json:"instanceTypes"`
	Regions       map[string]float64              `json:"regionMultipliers"`
}

func main() {
	// Parse command line flags
	config := parseFlags()

	// Initialize Kubernetes clients
	clientset, metricsClient := initKubernetesClients(config.KubeConfigPath)

	// Load pricing configuration
	pricing := loadPricingConfig(config.PricingConfigPath)

	// Create context
	ctx := context.Background()

	if config.OneShot {
		// Run once and exit
		if err := generateReport(ctx, clientset, metricsClient, pricing, config); err != nil {
			log.Fatalf("Failed to generate report: %v", err)
		}
		return
	}

	// Start metrics server for Prometheus
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Printf("Starting metrics server on port %d", config.MetricsPort)
		if err := http.ListenAndServe(fmt.Sprintf(":%d", config.MetricsPort), nil); err != nil {
			log.Fatalf("Failed to start metrics server: %v", err)
		}
	}()

	// Run continuous monitoring
	ticker := time.NewTicker(config.CheckInterval)
	defer ticker.Stop()

	// Run first check immediately
	if err := generateReport(ctx, clientset, metricsClient, pricing, config); err != nil {
		log.Printf("Failed to generate initial report: %v", err)
	}

	// Monitor continuously
	for range ticker.C {
		if err := generateReport(ctx, clientset, metricsClient, pricing, config); err != nil {
			log.Printf("Failed to generate report: %v", err)
		}
	}

	// Run resource optimization analysis
	optimizer := NewResourceOptimizer(clientset, metricsClient)
	report, err := optimizer.GenerateOptimizationReport(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Potential Monthly Savings: $%.2f\n", report.PotentialSavings)
	fmt.Printf("Optimization Recommendations: %d\n", len(report.Recommendations))

	for _, rec := range report.Recommendations {
		fmt.Printf("- %s: %s (Save $%.2f/month)\n",
			rec.Type, rec.Description, rec.PotentialSaving)
	}

	// Run cleanup with dry-run
	cleanupRecs, err := CleanupUnusedResources(context.Background(), clientset, true)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("\nCleanup Recommendations: %d\n", len(cleanupRecs))
	for _, rec := range cleanupRecs {
		fmt.Printf("- Delete %s %s/%s: %s\n",
			rec.ResourceType, rec.Namespace, rec.Name, rec.Reason)
	}
}

// parseFlags parses command line flags and returns configuration
func parseFlags() *Config {
	config := &Config{}

	// Default to $HOME/.kube/config for kubeconfig path
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to get user home directory: %v", err)
	}
	defaultKubeConfig := filepath.Join(homeDir, ".kube", "config")

	flag.StringVar(&config.KubeConfigPath, "kubeconfig", defaultKubeConfig, "Path to kubeconfig file")
	flag.StringVar(&config.PricingConfigPath, "pricing-config", "pricing-config.json", "Path to pricing configuration file")
	flag.StringVar((*string)(&config.ReportFormat), "format", string(reports.FormatText), "Report format (text, json, html)")
	flag.StringVar(&config.ReportPath, "output", "", "Output file path (empty for stdout)")
	flag.DurationVar(&config.CheckInterval, "interval", 60*time.Second, "Check interval for continuous monitoring")
	flag.IntVar(&config.MetricsPort, "metrics-port", 8085, "Prometheus metrics port")
	flag.BoolVar(&config.OneShot, "one-shot", false, "Run once and exit")
	flag.StringVar(&config.ReportType, "type", "combined", "Report type (health, cost, combined)")

	flag.Parse()
	return config
}

// initKubernetesClients initializes Kubernetes client and metrics client
func initKubernetesClients(kubeConfigPath string) (*kubernetes.Clientset, *metricsv.Clientset) {
	var config *rest.Config
	var err error

	// Try to use in-cluster config first
	config, err = rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig file
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfigPath)
		if err != nil {
			log.Fatalf("Failed to create Kubernetes config: %v", err)
		}
	}

	// Create clientset for Kubernetes API
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Create clientset for Metrics API
	metricsClient, err := versioned.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Metrics client: %v", err)
	}

	return clientset, metricsClient
}

// loadPricingConfig loads pricing configuration from file
func loadPricingConfig(configPath string) map[string]cost.ResourcePricing {
	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Create default pricing config
		pricingConfig := PricingConfig{
			Defaults: cost.ResourcePricing{
				CPU:     0.03,
				Memory:  0.004,
				Storage: 0.00012,
				Network: 0.08,
				GPUPricing: map[string]float64{
					"nvidia-tesla-v100": 1.2,
					"nvidia-tesla-k80":  0.6,
				},
			},
			InstanceTypes: map[string]cost.ResourcePricing{
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
				"r5.large": {
					CPU:     0.030,
					Memory:  0.0055,
					Storage: 0.00012,
					Network: 0.085,
				},
			},
			Regions: map[string]float64{
				"us-east-1":      1.0,
				"us-west-2":      1.05,
				"eu-west-1":      1.1,
				"ap-southeast-1": 1.15,
			},
		}

		// Save default config
		data, err := json.MarshalIndent(pricingConfig, "", "  ")
		if err != nil {
			log.Fatalf("Failed to marshal pricing config: %v", err)
		}

		if err := os.WriteFile(configPath, data, 0644); err != nil {
			log.Fatalf("Failed to write pricing config: %v", err)
		}

		log.Printf("Created default pricing configuration: %s", configPath)
		return convertPricingConfig(pricingConfig)
	}

	// Load existing config
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("Failed to read pricing config: %v", err)
	}

	var pricingConfig PricingConfig
	if err := json.Unmarshal(data, &pricingConfig); err != nil {
		log.Fatalf("Failed to parse pricing config: %v", err)
	}

	return convertPricingConfig(pricingConfig)
}

// convertPricingConfig converts PricingConfig to the format expected by cost utilities
func convertPricingConfig(config PricingConfig) map[string]cost.ResourcePricing {
	pricing := make(map[string]cost.ResourcePricing)

	// Add default pricing
	pricing["default"] = config.Defaults

	// Add instance-type specific pricing
	for instanceType, instancePricing := range config.InstanceTypes {
		pricing[instanceType] = instancePricing
	}

	return pricing
}

// generateReport generates the requested type of report
func generateReport(ctx context.Context, clientset *kubernetes.Clientset, metricsClient *metricsv.Clientset, pricing map[string]cost.ResourcePricing, config *Config) error {
	// Determine output writer
	var output io.Writer = os.Stdout
	if config.ReportPath != "" {
		file, err := os.Create(config.ReportPath)
		if err != nil {
			return fmt.Errorf("failed to create output file: %w", err)
		}
		defer file.Close()
		output = file
	}

	// Create report generator
	generator := reports.NewReportGenerator(clientset, metricsClient, config.ReportFormat, output)

	// Generate the appropriate report type
	switch config.ReportType {
	case "health":
		return generator.GenerateHealthReport(ctx)
	case "cost":
		return generator.GenerateCostReport(ctx, pricing)
	case "combined":
		return generator.GenerateCombinedReport(ctx, pricing)
	default:
		return fmt.Errorf("unknown report type: %s", config.ReportType)
	}
}

// Additional utility functions for advanced use cases

// RunHealthCheck runs a quick health check and returns summary
func RunHealthCheck(ctx context.Context, clientset *kubernetes.Clientset, metricsClient *metricsv.Clientset) (map[string]interface{}, error) {
	healthData, err := health.GetClusterHealth(ctx, clientset, metricsClient)
	if err != nil {
		return nil, err
	}

	summary := map[string]interface{}{
		"healthScore":    healthData.HealthScore,
		"readyNodes":     healthData.NodeStatus.ReadyNodes,
		"totalNodes":     healthData.NodeStatus.TotalNodes,
		"runningPods":    healthData.PodStatus.RunningPods,
		"totalPods":      healthData.PodStatus.TotalPods,
		"criticalIssues": len(filterIssuesBySeverity(healthData.Issues, "critical")),
		"warningIssues":  len(filterIssuesBySeverity(healthData.Issues, "warning")),
		"cpuUsage":       healthData.ResourceUsage.ClusterCPUUsage,
		"memoryUsage":    healthData.ResourceUsage.ClusterMemoryUsage,
	}

	return summary, nil
}

// filterIssuesBySeverity filters issues by their severity level
func filterIssuesBySeverity(issues []health.HealthIssue, severity string) []health.HealthIssue {
	filtered := make([]health.HealthIssue, 0)
	for _, issue := range issues {
		if issue.Severity == severity {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}

// Cost monitoring implementation was removed from the top-level file scope because it
// contained executable statements outside of any function; move cost monitoring or
// forecasting logic into a dedicated function (e.g. MonitorCosts) if needed.
// TODO: implement MonitorCosts(ctx, clientset, metricsClient, pricing) as appropriate.

// CleanupUnusedResources identifies and optionally cleans up unused resources
func CleanupUnusedResources(ctx context.Context, clientset *kubernetes.Clientset, dryRun bool) ([]CleanupRecommendation, error) {
	recommendations := make([]CleanupRecommendation, 0)

	// Find unused ConfigMaps
	configMaps, err := clientset.CoreV1().ConfigMaps("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list configmaps: %w", err)
	}

	pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Create a map of configmaps in use
	configMapsInUse := make(map[string]bool)
	for _, pod := range pods.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.ConfigMap != nil {
				key := fmt.Sprintf("%s/%s", pod.Namespace, volume.ConfigMap.Name)
				configMapsInUse[key] = true
			}
		}

		for _, container := range pod.Spec.Containers {
			for _, env := range container.Env {
				if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
					key := fmt.Sprintf("%s/%s", pod.Namespace, env.ValueFrom.ConfigMapKeyRef.Name)
					configMapsInUse[key] = true
				}
			}
		}
	}

	// Find unused configmaps
	for _, cm := range configMaps.Items {
		key := fmt.Sprintf("%s/%s", cm.Namespace, cm.Name)
		if !configMapsInUse[key] {
			rec := CleanupRecommendation{
				ResourceType: "ConfigMap",
				Namespace:    cm.Namespace,
				Name:         cm.Name,
				Reason:       "Not referenced by any pod",
				Age:          time.Since(cm.CreationTimestamp.Time),
			}
			recommendations = append(recommendations, rec)

			if !dryRun {
				// Delete unused configmap
				err := clientset.CoreV1().ConfigMaps(cm.Namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{})
				if err != nil {
					log.Printf("Failed to delete configmap %s/%s: %v", cm.Namespace, cm.Name, err)
				} else {
					log.Printf("Deleted unused configmap %s/%s", cm.Namespace, cm.Name)
				}
			}
		}
	}

	// Find failed pods older than 7 days
	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded {
			age := time.Since(pod.CreationTimestamp.Time)
			if age > 7*24*time.Hour {
				rec := CleanupRecommendation{
					ResourceType: "Pod",
					Namespace:    pod.Namespace,
					Name:         pod.Name,
					Reason:       fmt.Sprintf("Failed/Completed pod older than 7 days (status: %s)", pod.Status.Phase),
					Age:          age,
				}
				recommendations = append(recommendations, rec)

				if !dryRun {
					// Delete old failed/succeeded pod
					err := clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
					if err != nil {
						log.Printf("Failed to delete pod %s/%s: %v", pod.Namespace, pod.Name, err)
					} else {
						log.Printf("Deleted old pod %s/%s", pod.Namespace, pod.Name)
					}
				}
			}
		}
	}

	return recommendations, nil
}

// ResourceOptimizer provides recommendations for resource optimization
type ResourceOptimizer struct {
	clientset     *kubernetes.Clientset
	metricsClient *metricsv.Clientset
}

// NewResourceOptimizer creates a new resource optimizer
func NewResourceOptimizer(clientset *kubernetes.Clientset, metricsClient *metricsv.Clientset) *ResourceOptimizer {
	return &ResourceOptimizer{
		clientset:     clientset,
		metricsClient: metricsClient,
	}
}

// GenerateOptimizationReports generates comprehensive optimization recommendations
func (o *ResourceOptimizer) GenerateOptimizationReport(ctx context.Context) (*OptimizationReport, error) {
	report := &OptimizationReport{
		GeneratedAt:      time.Now(),
		Recommendations:  make([]OptimizationRecommendation, 0),
		PotentialSavings: 0.0,
	}

	// Get pod metrics
	podMetrics, err := o.metricsClient.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pod metrics: %w", err)
	}

	// Get pods
	pods, err := o.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pods: %w", err)
	}

	// Analyze each pod for optimization opportunities
	for _, pod := range pods.Items {
		// Find corresponding metrics
		var podMetricData *v1beta1.PodMetrics
		for _, metric := range podMetrics.Items {
			if metric.Name == pod.Name && metric.Namespace == pod.Namespace {
				podMetricData = &metric
				break
			}
		}

		if podMetricData == nil {
			continue // Skip pods without metrics
		}

		// Analyze resource requests vs actual usage
		for _, container := range pod.Spec.Containers {
			if container.Resources.Requests == nil {
				// Missing resource requests
				rec := OptimizationRecommendation{
					Type:            "Missing Resource Requests",
					ResourceType:    "CPU/Memory",
					Namespace:       pod.Namespace,
					PodName:         pod.Name,
					ContainerName:   container.Name,
					Description:     "Container has no resource requests defined",
					Suggestion:      "Add resource requests based on actual usage",
					PotentialSaving: 0.0,
				}
				report.Recommendations = append(report.Recommendations, rec)
				continue
			}

			// Find actual usage for this container
			var actualCPU, actualMemory int64
			for _, containerMetric := range podMetricData.Containers {
				if containerMetric.Name == container.Name {
					actualCPU = containerMetric.Usage.Cpu().MilliValue()
					actualMemory = containerMetric.Usage.Memory().Value()
					break
				}
			}

			// Check CPU over-provisioning
			if container.Resources.Requests.Cpu() != nil {
				requestedCPU := container.Resources.Requests.Cpu().MilliValue()
				if requestedCPU > 0 && actualCPU < requestedCPU/2 {
					// Using less than 50% of requested CPU
					rec := OptimizationRecommendation{
						Type:          "CPU Over-provisioning",
						ResourceType:  "CPU",
						Namespace:     pod.Namespace,
						PodName:       pod.Name,
						ContainerName: container.Name,
						Description: fmt.Sprintf("Using %dm CPU but requesting %dm",
							actualCPU, requestedCPU),
						Suggestion:      fmt.Sprintf("Reduce CPU request to %dm", actualCPU*2),
						PotentialSaving: calculateCPUSaving(requestedCPU - actualCPU*2),
					}
					report.Recommendations = append(report.Recommendations, rec)
					report.PotentialSavings += rec.PotentialSaving
				}
			}

			// Check memory over-provisioning
			if container.Resources.Requests.Memory() != nil {
				requestedMemory := container.Resources.Requests.Memory().Value()
				if requestedMemory > 0 && actualMemory < requestedMemory/2 {
					// Using less than 50% of requested memory
					rec := OptimizationRecommendation{
						Type:          "Memory Over-provisioning",
						ResourceType:  "Memory",
						Namespace:     pod.Namespace,
						PodName:       pod.Name,
						ContainerName: container.Name,
						Description: fmt.Sprintf("Using %dMi memory but requesting %dMi",
							actualMemory/(1024*1024), requestedMemory/(1024*1024)),
						Suggestion:      fmt.Sprintf("Reduce memory request to %dMi", actualMemory*2/(1024*1024)),
						PotentialSaving: calculateMemorySaving(requestedMemory - actualMemory*2),
					}
					report.Recommendations = append(report.Recommendations, rec)
					report.PotentialSavings += rec.PotentialSaving
				}
			}
		}
	}

	// Sort recommendations by potential savings
	sort.Slice(report.Recommendations, func(i, j int) bool {
		return report.Recommendations[i].PotentialSaving > report.Recommendations[j].PotentialSaving
	})

	return report, nil
}

// calculateCPUSaving calculates potential monthly savings for CPU reduction
func calculateCPUSaving(milliCPUReduction int64) float64 {
	cpuReduction := float64(milliCPUReduction) / 1000
	// Assume $0.03 per CPU core hour
	hourlySaving := cpuReduction * 0.03
	return hourlySaving * 24 * 30 // Monthly saving
}

// calculateMemorySaving calculates potential monthly savings for memory reduction
func calculateMemorySaving(memoryReductionBytes int64) float64 {
	memoryReductionGB := float64(memoryReductionBytes) / (1024 * 1024 * 1024)
	// Assume $0.004 per GB hour
	hourlySaving := memoryReductionGB * 0.004
	return hourlySaving * 24 * 30 // Monthly saving
}

// Type definitions for new functions
type CostForecast struct {
	CurrentMonthlyCost float64
	Months             int
	MonthlyForecasts   []MonthlyForecast
	ResourceBreakdown  map[string]float64
}

type MonthlyForecast struct {
	Month           int
	CostEstimate    float64
	ConfidenceLevel int // Percentage
}

type CleanupRecommendation struct {
	ResourceType string
	Namespace    string
	Name         string
	Reason       string
	Age          time.Duration
}

type OptimizationReport struct {
	GeneratedAt      time.Time
	Recommendations  []OptimizationRecommendation
	PotentialSavings float64
}

type OptimizationRecommendation struct {
	Type            string
	ResourceType    string
	Namespace       string
	PodName         string
	ContainerName   string
	Description     string
	Suggestion      string
	PotentialSaving float64
}
