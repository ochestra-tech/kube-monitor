package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/metrics/pkg/client/clientset/versioned"
)

// Configuration options
type Config struct {
	KubeConfigPath   string
	Interval         time.Duration
	MetricsPort      int
	OutputFile       string
	EnableCostReport bool
	PricingDataFile  string
}

// Cost data for different node types and regions
type PricingData struct {
	Nodes map[string]NodePricing `json:"nodes"`
}

type NodePricing struct {
	CPUCostPerHour     float64 `json:"cpuCostPerHour"`
	MemoryCostPerGBHr  float64 `json:"memoryCostPerGBHr"`
	StorageCostPerGBHr float64 `json:"storageCostPerGBHr"`
	RegionMultiplier   float64 `json:"regionMultiplier"`
}

// ClusterHealth represents the health status of the cluster
type ClusterHealth struct {
	TotalNodes              int     `json:"totalNodes"`
	ReadyNodes              int     `json:"readyNodes"`
	ResourceUtilization     float64 `json:"resourceUtilization"`
	PendingPods             int     `json:"pendingPods"`
	FailedPods              int     `json:"failedPods"`
	CriticalComponentsOK    bool    `json:"criticalComponentsOK"`
	MemoryPressureNodes     int     `json:"memoryPressureNodes"`
	DiskPressureNodes       int     `json:"diskPressureNodes"`
	PIDPressureNodes        int     `json:"pidPressureNodes"`
	NetworkUnavailableNodes int     `json:"networkUnavailableNodes"`
}

// CostReport represents the estimated costs for the cluster
type CostReport struct {
	TotalCostPerHour   float64               `json:"totalCostPerHour"`
	TotalCostPerMonth  float64               `json:"totalCostPerMonth"`
	CostByNamespace    map[string]float64    `json:"costByNamespace"`
	CostByNodeType     map[string]float64    `json:"costByNodeType"`
	EfficientWorkloads []string              `json:"efficientWorkloads"`
	Recommendations    []CostOptimizationRec `json:"recommendations"`
}

// CostOptimizationRec represents a cost optimization recommendation
type CostOptimizationRec struct {
	Type        string  `json:"type"`
	Resource    string  `json:"resource"`
	Namespace   string  `json:"namespace"`
	Description string  `json:"description"`
	Savings     float64 `json:"savings"`
}

// Prometheus metrics
var (
	nodeStatusGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_health_manager_node_status",
			Help: "Status of Kubernetes nodes (1=ready, 0=not ready)",
		},
		[]string{"node"},
	)

	podStatusGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_health_manager_pod_status",
			Help: "Status of Kubernetes pods (2=running, 1=pending, 0=failed)",
		},
		[]string{"namespace", "pod"},
	)

	namespaceResourceUsageGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_health_manager_namespace_resource_usage",
			Help: "Resource usage by namespace",
		},
		[]string{"namespace", "resource_type"},
	)

	namespaceCostGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_health_manager_namespace_cost",
			Help: "Estimated cost per namespace per hour",
		},
		[]string{"namespace"},
	)

	resourceEfficiencyGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_health_manager_resource_efficiency",
			Help: "Resource efficiency (usage/requests ratio)",
		},
		[]string{"namespace", "resource_type"},
	)
)

func init() {
	// Register Prometheus metrics
	prometheus.MustRegister(nodeStatusGauge)
	prometheus.MustRegister(podStatusGauge)
	prometheus.MustRegister(namespaceResourceUsageGauge)
	prometheus.MustRegister(namespaceCostGauge)
	prometheus.MustRegister(resourceEfficiencyGauge)
}

func main() {

	config := parseCommandLineFlags()

	// Start metrics server
	startMetricsServer(config.MetricsPort)

	// Load pricing data for cost estimation
	pricingData := loadPricingData(config.PricingDataFile)

	// Initialize Kubernetes client
	clientset, metricsClient := initKubernetesClient(config.KubeConfigPath)

	// Run continuous health and cost checks
	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()

	for {
		// Check cluster health
		health := checkClusterHealth(clientset)

		// Generate cost report if enabled
		var costReport *CostReport
		if config.EnableCostReport {
			costReport = generateCostReport(clientset, metricsClient, pricingData)
		}

		// Update Prometheus metrics
		updateMetrics(clientset, metricsClient)

		// Output results
		if config.OutputFile != "" {
			outputResults(config.OutputFile, health, costReport)
		}

		// Print summary to stdout
		printSummary(health, costReport)

		// Wait for next interval
		<-ticker.C
	}
}

func parseCommandLineFlags() *Config {
	config := &Config{}

	// Default to $HOME/.kube/config for kubeconfig path
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to get user home directory: %v", err)
	}
	defaultKubeConfig := filepath.Join(homeDir, ".kube", "config")

	flag.StringVar(&config.KubeConfigPath, "kubeconfig", defaultKubeConfig, "Path to kubeconfig file")
	flag.DurationVar(&config.Interval, "interval", 60*time.Second, "Check interval in seconds")
	flag.IntVar(&config.MetricsPort, "metrics-port", 8080, "Prometheus metrics port")
	flag.StringVar(&config.OutputFile, "output", "", "Output file for health and cost reports")
	flag.BoolVar(&config.EnableCostReport, "cost", true, "Enable cost reporting")
	flag.StringVar(&config.PricingDataFile, "pricing", "pricing.json", "Pricing data file")

	flag.Parse()
	return config
}

func startMetricsServer(port int) {
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Printf("Starting metrics server on port %d", port)
		if err := http.ListenAndServe(fmt.Sprintf(":%d", port), nil); err != nil {
			log.Fatalf("Failed to start metrics server: %v", err)
		}
	}()
}

func loadPricingData(filename string) *PricingData {
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		// Create default pricing data if file doesn't exist
		pricingData := &PricingData{
			Nodes: map[string]NodePricing{
				"default": {
					CPUCostPerHour:     0.03,
					MemoryCostPerGBHr:  0.004,
					StorageCostPerGBHr: 0.00012,
					RegionMultiplier:   1.0,
				},
				"highcpu": {
					CPUCostPerHour:     0.05,
					MemoryCostPerGBHr:  0.003,
					StorageCostPerGBHr: 0.00015,
					RegionMultiplier:   1.0,
				},
				"highmem": {
					CPUCostPerHour:     0.02,
					MemoryCostPerGBHr:  0.006,
					StorageCostPerGBHr: 0.0001,
					RegionMultiplier:   1.0,
				},
			},
		}

		// Write default pricing data to file
		data, err := json.MarshalIndent(pricingData, "", "  ")
		if err != nil {
			log.Fatalf("Failed to marshal pricing data: %v", err)
		}

		if err := os.WriteFile(filename, data, 0644); err != nil {
			log.Fatalf("Failed to write pricing data: %v", err)
		}

		log.Printf("Created default pricing data file: %s", filename)
		return pricingData
	}

	// Load pricing data from file
	data, err := os.ReadFile(filename)
	if err != nil {
		log.Fatalf("Failed to read pricing data: %v", err)
	}

	var pricingData PricingData
	if err := json.Unmarshal(data, &pricingData); err != nil {
		log.Fatalf("Failed to parse pricing data: %v", err)
	}

	return &pricingData
}

func initKubernetesClient(kubeConfigPath string) (*kubernetes.Clientset, *versioned.Clientset) {
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

func checkClusterHealth(clientset *kubernetes.Clientset) *ClusterHealth {
	ctx := context.Background()
	health := &ClusterHealth{}

	// Check nodes status
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Failed to list nodes: %v", err)
		return health
	}

	health.TotalNodes = len(nodes.Items)

	for _, node := range nodes.Items {
		for _, condition := range node.Status.Conditions {
			if condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue {
				health.ReadyNodes++
			}
			if condition.Type == v1.NodeMemoryPressure && condition.Status == v1.ConditionTrue {
				health.MemoryPressureNodes++
			}
			if condition.Type == v1.NodeDiskPressure && condition.Status == v1.ConditionTrue {
				health.DiskPressureNodes++
			}
			if condition.Type == v1.NodePIDPressure && condition.Status == v1.ConditionTrue {
				health.PIDPressureNodes++
			}
			if condition.Type == v1.NodeNetworkUnavailable && condition.Status == v1.ConditionTrue {
				health.NetworkUnavailableNodes++
			}
		}
	}

	// Check pod status
	pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Failed to list pods: %v", err)
		return health
	}

	for _, pod := range pods.Items {
		switch pod.Status.Phase {
		case v1.PodPending:
			health.PendingPods++
		case v1.PodFailed:
			health.FailedPods++
		}
	}

	// Check critical components in kube-system
	criticalNamespaces := []string{"kube-system", "kube-public"}
	health.CriticalComponentsOK = true

	for _, namespace := range criticalNamespaces {
		namespacePods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("Failed to list pods in %s: %v", namespace, err)
			health.CriticalComponentsOK = false
			continue
		}

		for _, pod := range namespacePods.Items {
			if pod.Status.Phase != v1.PodRunning {
				health.CriticalComponentsOK = false
				log.Printf("Critical component not running: %s/%s", namespace, pod.Name)
			}
		}
	}

	// Calculate resource utilization (simplified - would be more detailed with metrics-server data)
	if health.TotalNodes > 0 {
		health.ResourceUtilization = float64(len(pods.Items)) / float64(health.TotalNodes*110) * 100 // Assuming ~100 pods per node is "full"
	}

	return health
}

func generateCostReport(clientset *kubernetes.Clientset, metricsClient *versioned.Clientset, pricingData *PricingData) *CostReport {
	ctx := context.Background()
	costReport := &CostReport{
		CostByNamespace: make(map[string]float64),
		CostByNodeType:  make(map[string]float64),
		Recommendations: make([]CostOptimizationRec, 0),
	}

	// Get nodes info
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Failed to list nodes: %v", err)
		return costReport
	}

	// Calculate cost by node type
	for _, node := range nodes.Items {
		nodeType := "default"
		if t, ok := node.Labels["node.kubernetes.io/instance-type"]; ok {
			nodeType = t
		}

		// Find pricing for this node type, or use default
		pricing := pricingData.Nodes["default"]
		if p, ok := pricingData.Nodes[nodeType]; ok {
			pricing = p
		}

		// Calculate CPU capacity
		cpuCapacity := float64(node.Status.Capacity.Cpu().Value())
		cpuCost := cpuCapacity * pricing.CPUCostPerHour

		// Calculate memory capacity in GB
		memCapacity := float64(node.Status.Capacity.Memory().Value()) / (1024 * 1024 * 1024)
		memCost := memCapacity * pricing.MemoryCostPerGBHr

		// Apply region multiplier from pricing data
		nodeCost := (cpuCost + memCost) * pricing.RegionMultiplier

		if _, ok := costReport.CostByNodeType[nodeType]; !ok {
			costReport.CostByNodeType[nodeType] = 0
		}
		costReport.CostByNodeType[nodeType] += nodeCost
		costReport.TotalCostPerHour += nodeCost
	}

	// Get pods info
	pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Failed to list pods: %v", err)
		return costReport
	}

	// Create map to store namespace usage
	namespaceCPURequests := make(map[string]float64)
	namespaceMemRequests := make(map[string]float64)
	namespaceCPULimits := make(map[string]float64)
	namespaceMemLimits := make(map[string]float64)

	// Calculate usage by namespace
	for _, pod := range pods.Items {
		namespace := pod.Namespace

		if _, ok := namespaceCPURequests[namespace]; !ok {
			namespaceCPURequests[namespace] = 0
			namespaceMemRequests[namespace] = 0
			namespaceCPULimits[namespace] = 0
			namespaceMemLimits[namespace] = 0
		}

		// Sum up resource requests and limits
		for _, container := range pod.Spec.Containers {
			if container.Resources.Requests != nil {
				namespaceCPURequests[namespace] += float64(container.Resources.Requests.Cpu().MilliValue()) / 1000
				namespaceMemRequests[namespace] += float64(container.Resources.Requests.Memory().Value()) / (1024 * 1024 * 1024)
			}

			if container.Resources.Limits != nil {
				namespaceCPULimits[namespace] += float64(container.Resources.Limits.Cpu().MilliValue()) / 1000
				namespaceMemLimits[namespace] += float64(container.Resources.Limits.Memory().Value()) / (1024 * 1024 * 1024)
			}
		}
	}

	// Calculate cost by namespace
	totalClusterCPU := 0.0
	totalClusterMem := 0.0

	for _, node := range nodes.Items {
		totalClusterCPU += float64(node.Status.Capacity.Cpu().Value())
		totalClusterMem += float64(node.Status.Capacity.Memory().Value()) / (1024 * 1024 * 1024)
	}

	// Distribute cost to namespaces based on resource requests
	for namespace, cpuRequests := range namespaceCPURequests {
		cpuCostShare := 0.0
		memCostShare := 0.0

		if totalClusterCPU > 0 {
			cpuCostShare = cpuRequests / totalClusterCPU * costReport.TotalCostPerHour * 0.7 // Assuming CPU is 70% of cost
		}

		if totalClusterMem > 0 {
			memCostShare = namespaceMemRequests[namespace] / totalClusterMem * costReport.TotalCostPerHour * 0.3 // Assuming memory is 30% of cost
		}

		costReport.CostByNamespace[namespace] = cpuCostShare + memCostShare

		// Generate optimization recommendations
		generateOptimizationRecs(namespace, cpuRequests, namespaceMemRequests[namespace],
			namespaceCPULimits[namespace], namespaceMemLimits[namespace], costReport)
	}

	// Calculate monthly cost projection
	costReport.TotalCostPerMonth = costReport.TotalCostPerHour * 24 * 30

	return costReport
}

func generateOptimizationRecs(namespace string, cpuReq, memReq, cpuLimit, memLimit float64, costReport *CostReport) {
	// Check for missing resource requests
	if cpuReq == 0 && memReq == 0 {
		costReport.Recommendations = append(costReport.Recommendations, CostOptimizationRec{
			Type:        "Resource Requests",
			Resource:    "CPU/Memory",
			Namespace:   namespace,
			Description: "Missing resource requests, could lead to scheduling issues",
			Savings:     0,
		})
	}

	// Check for over-provisioned resources (large difference between requests and limits)
	if cpuLimit > 0 && cpuReq > 0 && cpuLimit/cpuReq > 4 {
		cpuSavings := (cpuLimit/cpuReq - 2) * cpuReq * 0.03 * 24 * 30 // Assuming $0.03 per CPU hour
		costReport.Recommendations = append(costReport.Recommendations, CostOptimizationRec{
			Type:        "Resource Optimization",
			Resource:    "CPU",
			Namespace:   namespace,
			Description: fmt.Sprintf("CPU limits are %.1fx higher than requests", cpuLimit/cpuReq),
			Savings:     cpuSavings,
		})
	}

	if memLimit > 0 && memReq > 0 && memLimit/memReq > 3 {
		memSavings := (memLimit/memReq - 1.5) * memReq * 0.004 * 24 * 30 // Assuming $0.004 per GB hour
		costReport.Recommendations = append(costReport.Recommendations, CostOptimizationRec{
			Type:        "Resource Optimization",
			Resource:    "Memory",
			Namespace:   namespace,
			Description: fmt.Sprintf("Memory limits are %.1fx higher than requests", memLimit/memReq),
			Savings:     memSavings,
		})
	}

	// Check for efficient workloads
	if cpuReq > 0 && memReq > 0 && cpuLimit > 0 && memLimit > 0 {
		if cpuLimit/cpuReq <= 2 && memLimit/memReq <= 2 {
			costReport.EfficientWorkloads = append(costReport.EfficientWorkloads, namespace)
		}
	}
}

func updateMetrics(clientset *kubernetes.Clientset, metricsClient *versioned.Clientset) {
	ctx := context.Background()

	// Update node status metrics
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Failed to list nodes for metrics: %v", err)
		return
	}

	for _, node := range nodes.Items {
		isReady := 0.0
		for _, condition := range node.Status.Conditions {
			if condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue {
				isReady = 1.0
				break
			}
		}
		nodeStatusGauge.WithLabelValues(node.Name).Set(isReady)
	}

	// Update pod status metrics
	pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Failed to list pods for metrics: %v", err)
		return
	}

	for _, pod := range pods.Items {
		var statusValue float64

		switch pod.Status.Phase {
		case v1.PodRunning:
			statusValue = 2.0
		case v1.PodPending:
			statusValue = 1.0
		case v1.PodFailed:
			statusValue = 0.0
		case v1.PodSucceeded:
			statusValue = 3.0
		default:
			statusValue = -1.0
		}

		podStatusGauge.WithLabelValues(pod.Namespace, pod.Name).Set(statusValue)
	}

	// Aggregate resource usage by namespace
	namespaceCPUUsage := make(map[string]float64)
	namespaceMemUsage := make(map[string]float64)
	namespaceCPURequests := make(map[string]float64)
	namespaceMemRequests := make(map[string]float64)

	for _, pod := range pods.Items {
		namespace := pod.Namespace

		if _, ok := namespaceCPUUsage[namespace]; !ok {
			namespaceCPUUsage[namespace] = 0
			namespaceMemUsage[namespace] = 0
			namespaceCPURequests[namespace] = 0
			namespaceMemRequests[namespace] = 0
		}

		// Sum resource requests
		for _, container := range pod.Spec.Containers {
			if container.Resources.Requests != nil {
				cpuReq := float64(container.Resources.Requests.Cpu().MilliValue()) / 1000
				memReq := float64(container.Resources.Requests.Memory().Value()) / (1024 * 1024 * 1024)

				namespaceCPURequests[namespace] += cpuReq
				namespaceMemRequests[namespace] += memReq
			}
		}
	}

	// Update namespace resource usage metrics
	for namespace, cpuUsage := range namespaceCPUUsage {
		namespaceResourceUsageGauge.WithLabelValues(namespace, "cpu").Set(cpuUsage)
		namespaceResourceUsageGauge.WithLabelValues(namespace, "memory").Set(namespaceMemUsage[namespace])

		// Update resource efficiency metrics if we have both usage and requests
		if namespaceCPURequests[namespace] > 0 {
			cpuEfficiency := cpuUsage / namespaceCPURequests[namespace]
			resourceEfficiencyGauge.WithLabelValues(namespace, "cpu").Set(cpuEfficiency)
		}

		if namespaceMemRequests[namespace] > 0 {
			memEfficiency := namespaceMemUsage[namespace] / namespaceMemRequests[namespace]
			resourceEfficiencyGauge.WithLabelValues(namespace, "memory").Set(memEfficiency)
		}
	}
}

func outputResults(filename string, health *ClusterHealth, costReport *CostReport) {
	output := struct {
		Timestamp  string         `json:"timestamp"`
		Health     *ClusterHealth `json:"health"`
		CostReport *CostReport    `json:"costReport,omitempty"`
	}{
		Timestamp:  time.Now().Format(time.RFC3339),
		Health:     health,
		CostReport: costReport,
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		log.Printf("Failed to marshal output: %v", err)
		return
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		log.Printf("Failed to write output: %v", err)
	}
}

func printSummary(health *ClusterHealth, costReport *CostReport) {
	fmt.Println("=== Kubernetes Health and Cost Management Summary ===")
	fmt.Printf("Time: %s\n\n", time.Now().Format(time.RFC3339))

	fmt.Println("--- Cluster Health ---")
	fmt.Printf("Nodes: %d total, %d ready\n", health.TotalNodes, health.ReadyNodes)
	fmt.Printf("Resource Utilization: %.1f%%\n", health.ResourceUtilization)
	fmt.Printf("Pod Issues: %d pending, %d failed\n", health.PendingPods, health.FailedPods)
	fmt.Printf("Critical Components: %v\n", health.CriticalComponentsOK)
	fmt.Printf("Pressure Conditions: %d memory, %d disk, %d PID, %d network\n",
		health.MemoryPressureNodes, health.DiskPressureNodes, health.PIDPressureNodes, health.NetworkUnavailableNodes)

	if costReport != nil {
		fmt.Println("\n--- Cost Report ---")
		fmt.Printf("Total Cost: $%.2f/hour, $%.2f/month\n", costReport.TotalCostPerHour, costReport.TotalCostPerMonth)

		fmt.Println("\nTop 5 Namespace Costs:")
		count := 0
		for namespace, cost := range costReport.CostByNamespace {
			fmt.Printf("  %s: $%.2f/hour\n", namespace, cost)
			count++
			if count >= 5 {
				break
			}
		}

		fmt.Println("\nCost Recommendations:")
		for i, rec := range costReport.Recommendations {
			if i >= 3 {
				break
			}
			fmt.Printf("  [%s/%s] %s - Potential savings: $%.2f/month\n",
				rec.Namespace, rec.Resource, rec.Description, rec.Savings)
		}
	}

	fmt.Println("\n=====================================================")
}
