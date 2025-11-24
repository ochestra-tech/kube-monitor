package reports

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/ochestra-tech/k8s-monitor/pkg/cost"
	"github.com/ochestra-tech/k8s-monitor/pkg/health"
)

// ReportFormat specifies the output format for reports
type ReportFormat string

const (
	FormatJSON ReportFormat = "json"
	FormatCSV  ReportFormat = "csv"
	FormatHTML ReportFormat = "html"
	FormatText ReportFormat = "text"

	combinedReportHTMLTemplate = `
<!DOCTYPE html>
<html>
<head>
	<title>Kubernetes Combined Report</title>
</head>
<body>
	<h1>Kubernetes Cluster Combined Report</h1>
	<p>Generated at: {{.Timestamp}}</p>
	
	<h2>Health Status</h2>
	<p>Health Score: {{.Health.HealthScore}}/100</p>
	
	<h2>Cost Summary</h2>
	<table>
		<tr>
			<th>Node</th>
			<th>Instance Type</th>
			<th>Total Cost</th>
		</tr>
		{{range .Nodes}}
		<tr>
			<td>{{.Name}}</td>
			<td>{{.InstanceType}}</td>
			<td>${{printf "%.2f" .TotalCost}}</td>
		</tr>
		{{end}}
	</table>
</body>
</html>`

	healthReportHTMLTemplate = `
<!DOCTYPE html>
<html>
<head>
	<title>Kubernetes Health Report</title>
</head>
<body>
	<h1>Kubernetes Cluster Health Report</h1>
	<p>Generated at: {{.Timestamp}}</p>
	<p>Health Score: {{.HealthScore}}/100</p>
	
	<h2>Node Status</h2>
	<ul>
		<li>Total Nodes: {{.NodeStatus.TotalNodes}}</li>
		<li>Ready Nodes: {{.NodeStatus.ReadyNodes}}</li>
	</ul>
</body>
</html>`

	costReportHTMLTemplate = `
<!DOCTYPE html>
<html>
<head>
	<title>Kubernetes Cost Report</title>
</head>
<body>
	<h1>Kubernetes Cluster Cost Report</h1>
	<p>Generated at: {{.Timestamp}}</p>
	
	<h2>Node Costs</h2>
	<table>
		<tr>
			<th>Node</th>
			<th>Instance Type</th>
			<th>Total Cost</th>
			<th>CPU Cost</th>
			<th>Memory Cost</th>
		</tr>
		{{range .Nodes}}
		<tr>
			<td>{{.Name}}</td>
			<td>{{.InstanceType}}</td>
			<td>${{printf "%.2f" .TotalCost}}</td>
			<td>${{printf "%.2f" .CPUCost}}</td>
			<td>${{printf "%.2f" .MemoryCost}}</td>
		</tr>
		{{end}}
	</table>
</body>
</html>`
)

// ReportGenerator handles report generation
type ReportGenerator struct {
	clientset     *kubernetes.Clientset
	metricsClient *metricsv.Clientset
	format        ReportFormat
	writer        io.Writer
}

// NewReportGenerator creates a new report generator
func NewReportGenerator(clientset *kubernetes.Clientset, metricsClient *metricsv.Clientset, format ReportFormat, writer io.Writer) *ReportGenerator {
	return &ReportGenerator{
		clientset:     clientset,
		metricsClient: metricsClient,
		format:        format,
		writer:        writer,
	}
}

// GenerateHealthReport generates a comprehensive health report
func (r *ReportGenerator) GenerateHealthReport(ctx context.Context) error {
	healthData, err := health.GetClusterHealth(ctx, r.clientset, r.metricsClient)
	if err != nil {
		return fmt.Errorf("failed to get cluster health: %w", err)
	}

	switch r.format {
	case FormatJSON:
		return r.generateHealthReportJSON(healthData)
	case FormatHTML:
		return r.generateHealthReportHTML(healthData)
	case FormatText:
		return r.generateHealthReportText(healthData)
	default:
		return fmt.Errorf("unsupported format: %s", r.format)
	}
}

// GenerateCostReport generates a comprehensive cost report
func (r *ReportGenerator) GenerateCostReport(ctx context.Context, pricing map[string]cost.ResourcePricing) error {
	// Get pod costs
	podCosts, err := cost.GetPodCosts(ctx, r.clientset, r.metricsClient, pricing)
	if err != nil {
		return fmt.Errorf("failed to get pod costs: %w", err)
	}

	// Get node costs
	nodeCosts, err := cost.GetNodeCosts(ctx, r.clientset, r.metricsClient, pricing)
	if err != nil {
		return fmt.Errorf("failed to get node costs: %w", err)
	}

	// Get namespace costs
	namespaceCosts := cost.GetNamespaceCosts(podCosts)

	switch r.format {
	case FormatJSON:
		return r.generateCostReportJSON(podCosts, nodeCosts, namespaceCosts)
	case FormatHTML:
		return r.generateCostReportHTML(podCosts, nodeCosts, namespaceCosts)
	case FormatText:
		return r.generateCostReportText(podCosts, nodeCosts, namespaceCosts)
	default:
		return fmt.Errorf("unsupported format: %s", r.format)
	}
}

// GenerateCombinedReport generates a combined health and cost report
func (r *ReportGenerator) GenerateCombinedReport(ctx context.Context, pricing map[string]cost.ResourcePricing) error {
	// Get health data
	healthData, err := health.GetClusterHealth(ctx, r.clientset, r.metricsClient)
	if err != nil {
		return fmt.Errorf("failed to get cluster health: %w", err)
	}

	// Get cost data
	podCosts, err := cost.GetPodCosts(ctx, r.clientset, r.metricsClient, pricing)
	if err != nil {
		return fmt.Errorf("failed to get pod costs: %w", err)
	}

	nodeCosts, err := cost.GetNodeCosts(ctx, r.clientset, r.metricsClient, pricing)
	if err != nil {
		return fmt.Errorf("failed to get node costs: %w", err)
	}

	namespaceCosts := cost.GetNamespaceCosts(podCosts)

	switch r.format {
	case FormatJSON:
		return r.generateCombinedReportJSON(healthData, podCosts, nodeCosts, namespaceCosts)
	case FormatHTML:
		return r.generateCombinedReportHTML(healthData, podCosts, nodeCosts, namespaceCosts)
	case FormatText:
		return r.generateCombinedReportText(healthData, podCosts, nodeCosts, namespaceCosts)
	default:
		return fmt.Errorf("unsupported format: %s", r.format)
	}
}

// generateHealthReportJSON generates a JSON health report
func (r *ReportGenerator) generateHealthReportJSON(healthData *health.ClusterHealth) error {
	encoder := json.NewEncoder(r.writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(healthData)
}

// generateHealthReportHTML generates an HTML health report
func (r *ReportGenerator) generateHealthReportHTML(healthData *health.ClusterHealth) error {
	tmpl := template.Must(template.New("health").Parse(healthReportHTMLTemplate))
	return tmpl.Execute(r.writer, healthData)
}

// generateCostReportJSON generates a JSON cost report
func (r *ReportGenerator) generateCostReportJSON(podCosts []cost.PodCostData, nodeCosts []cost.NodeCostData, namespaceCosts []cost.NamespaceCostData) error {
	data := struct {
		Pods       []cost.PodCostData       `json:"pods"`
		Nodes      []cost.NodeCostData      `json:"nodes"`
		Namespaces []cost.NamespaceCostData `json:"namespaces"`
		Timestamp  time.Time                `json:"timestamp"`
	}{
		Pods:       podCosts,
		Nodes:      nodeCosts,
		Namespaces: namespaceCosts,
		Timestamp:  time.Now(),
	}
	encoder := json.NewEncoder(r.writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(data)
}

// generateHealthReportText generates a text health report
func (r *ReportGenerator) generateHealthReportText(healthData *health.ClusterHealth) error {
	fmt.Fprintf(r.writer, "=== Kubernetes Cluster Health Report ===\n")
	fmt.Fprintf(r.writer, "Generated at: %s\n\n", healthData.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(r.writer, "Overall Health Score: %d/100\n\n", healthData.HealthScore)

	// Node Health Summary
	fmt.Fprintf(r.writer, "--- Node Health ---\n")
	fmt.Fprintf(r.writer, "Total Nodes:                    %d\n", healthData.NodeStatus.TotalNodes)
	fmt.Fprintf(r.writer, "Ready Nodes:                    %d\n", healthData.NodeStatus.ReadyNodes)
	fmt.Fprintf(r.writer, "Memory Pressure Nodes:          %d\n", healthData.NodeStatus.MemoryPressureNodes)
	fmt.Fprintf(r.writer, "Disk Pressure Nodes:            %d\n", healthData.NodeStatus.DiskPressureNodes)
	fmt.Fprintf(r.writer, "PID Pressure Nodes:             %d\n", healthData.NodeStatus.PIDPressureNodes)
	fmt.Fprintf(r.writer, "Network Unavailable Nodes:      %d\n", healthData.NodeStatus.NetworkUnavailableNodes)
	fmt.Fprintf(r.writer, "Average Node Load:              %.2f\n\n", healthData.NodeStatus.AverageLoad)

	// Pod Health Summary
	fmt.Fprintf(r.writer, "--- Pod Health ---\n")
	fmt.Fprintf(r.writer, "Total Pods:                     %d\n", healthData.PodStatus.TotalPods)
	fmt.Fprintf(r.writer, "Running Pods:                   %d\n", healthData.PodStatus.RunningPods)
	fmt.Fprintf(r.writer, "Pending Pods:                   %d\n", healthData.PodStatus.PendingPods)
	fmt.Fprintf(r.writer, "Failed Pods:                    %d\n", healthData.PodStatus.FailedPods)
	fmt.Fprintf(r.writer, "Restarting Pods:                %d\n", healthData.PodStatus.RestartingPods)
	fmt.Fprintf(r.writer, "Crash Looping Pods:             %d\n\n", len(healthData.PodStatus.CrashLoopingPods))

	// Control Plane Status
	fmt.Fprintf(r.writer, "--- Control Plane Status ---\n")
	fmt.Fprintf(r.writer, "API Server Healthy:             %v\n", healthData.ControlPlaneStatus.APIServerHealthy)
	fmt.Fprintf(r.writer, "Controller Manager Healthy:     %v\n", healthData.ControlPlaneStatus.ControllerHealthy)
	fmt.Fprintf(r.writer, "Scheduler Healthy:              %v\n", healthData.ControlPlaneStatus.SchedulerHealthy)
	fmt.Fprintf(r.writer, "Etcd Healthy:                   %v\n", healthData.ControlPlaneStatus.EtcdHealthy)
	fmt.Fprintf(r.writer, "CoreDNS Healthy:                %v\n", healthData.ControlPlaneStatus.CoreDNSHealthy)
	fmt.Fprintf(r.writer, "API Server Latency:             %.2f ms\n\n", healthData.ControlPlaneStatus.APIServerLatency)

	// Resource Usage
	fmt.Fprintf(r.writer, "--- Resource Usage ---\n")
	fmt.Fprintf(r.writer, "Cluster CPU Usage:              %.1f%%\n", healthData.ResourceUsage.ClusterCPUUsage)
	fmt.Fprintf(r.writer, "Cluster Memory Usage:           %.1f%%\n", healthData.ResourceUsage.ClusterMemoryUsage)
	fmt.Fprintf(r.writer, "Cluster Storage Usage:          %.1f%%\n\n", healthData.ResourceUsage.ClusterStorageUsage)

	// Health Issues
	if len(healthData.Issues) > 0 {
		fmt.Fprintf(r.writer, "--- Health Issues ---\n")
		for i, issue := range healthData.Issues {
			if i >= 10 { // Limit to top 10 issues
				break
			}
			fmt.Fprintf(r.writer, "[%s] %s: %s\n", issue.Severity, issue.Resource, issue.Message)
			if issue.Suggestion != "" {
				fmt.Fprintf(r.writer, "Suggestion: %s\n", issue.Suggestion)
			}
		}
	}

	return nil
}

// generateCombinedReportText generates a text combined report
func (r *ReportGenerator) generateCombinedReportText(healthData *health.ClusterHealth, podCosts []cost.PodCostData, nodeCosts []cost.NodeCostData, namespaceCosts []cost.NamespaceCostData) error {
	// Print health report
	if err := r.generateHealthReportText(healthData); err != nil {
		return err
	}

	fmt.Fprintf(r.writer, "\n\n")

	// Print cost report
	return r.generateCostReportText(podCosts, nodeCosts, namespaceCosts)
}

// generateCombinedReportJSON generates a JSON combined report
func (r *ReportGenerator) generateCombinedReportJSON(healthData *health.ClusterHealth, podCosts []cost.PodCostData, nodeCosts []cost.NodeCostData, namespaceCosts []cost.NamespaceCostData) error {
	combinedReport := struct {
		Health     *health.ClusterHealth    `json:"health"`
		Pods       []cost.PodCostData       `json:"pods"`
		Nodes      []cost.NodeCostData      `json:"nodes"`
		Namespaces []cost.NamespaceCostData `json:"namespaces"`
	}{
		Health:     healthData,
		Pods:       podCosts,
		Nodes:      nodeCosts,
		Namespaces: namespaceCosts,
	}

	encoder := json.NewEncoder(r.writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(combinedReport)
}

// generateCostReportHTML generates an HTML cost report
func (r *ReportGenerator) generateCostReportHTML(podCosts []cost.PodCostData, nodeCosts []cost.NodeCostData, namespaceCosts []cost.NamespaceCostData) error {
	tmpl := template.Must(template.New("cost").Parse(costReportHTMLTemplate))
	data := struct {
		Pods       []cost.PodCostData
		Nodes      []cost.NodeCostData
		Namespaces []cost.NamespaceCostData
		Timestamp  time.Time
	}{
		Pods:       podCosts,
		Nodes:      nodeCosts,
		Namespaces: namespaceCosts,
		Timestamp:  time.Now(),
	}
	return tmpl.Execute(r.writer, data)
}

// generateCombinedReportHTML generates an HTML combined report
func (r *ReportGenerator) generateCombinedReportHTML(healthData *health.ClusterHealth, podCosts []cost.PodCostData, nodeCosts []cost.NodeCostData, namespaceCosts []cost.NamespaceCostData) error {
	tmpl := template.Must(template.New("combined").Parse(combinedReportHTMLTemplate))
	data := struct {
		Health     *health.ClusterHealth
		Pods       []cost.PodCostData
		Nodes      []cost.NodeCostData
		Namespaces []cost.NamespaceCostData
		Timestamp  time.Time
	}{
		Health:     healthData,
		Pods:       podCosts,
		Nodes:      nodeCosts,
		Namespaces: namespaceCosts,
		Timestamp:  time.Now(),
	}
	return tmpl.Execute(r.writer, data)
}

// generateCostReportText generates a text cost report
func (r *ReportGenerator) generateCostReportText(podCosts []cost.PodCostData, nodeCosts []cost.NodeCostData, namespaceCosts []cost.NamespaceCostData) error {
	fmt.Fprintf(r.writer, "=== Kubernetes Cluster Cost Report ===\n")
	fmt.Fprintf(r.writer, "Generated at: %s\n\n", time.Now().Format(time.RFC3339))

	// Calculate totals
	totalHourlyCost := 0.0
	for _, node := range nodeCosts {
		totalHourlyCost += node.TotalCost
	}
	totalMonthlyCost := totalHourlyCost * 24 * 30

	fmt.Fprintf(r.writer, "Total Hourly Cost:              $%.2f\n", totalHourlyCost)
	fmt.Fprintf(r.writer, "Total Monthly Cost:             $%.2f\n\n", totalMonthlyCost)

	// Node Cost Summary
	fmt.Fprintf(r.writer, "--- Node Cost Summary ---\n")

	// Sort by cost descending
	sort.Slice(nodeCosts, func(i, j int) bool {
		return nodeCosts[i].TotalCost > nodeCosts[j].TotalCost
	})

	// Print header
	fmt.Fprintf(r.writer, "%-36s %-20s %12s %12s %12s %12s\n",
		"Node", "Instance Type", "Hourly Cost", "CPU Cost", "Memory Cost", "Utilization")
	fmt.Fprintf(r.writer, "%s\n", strings.Repeat("-", 110))

	// Print rows (limit to top 10)
	for i, node := range nodeCosts {
		if i >= 10 { // Limit to top 10 nodes
			break
		}
		fmt.Fprintf(r.writer, "%-36s %-20s %12s %12s %12s %11s\n",
			node.Name,
			node.InstanceType,
			fmt.Sprintf("$%.2f", node.TotalCost),
			fmt.Sprintf("$%.2f", node.CPUCost),
			fmt.Sprintf("$%.2f", node.MemoryCost),
			fmt.Sprintf("%.1f%%", node.Utilization),
		)
	}

	// Namespace Cost Summary
	fmt.Fprintf(r.writer, "--- Namespace Cost Summary ---\n")

	// Print header
	fmt.Fprintf(r.writer, "%-30s %12s %10s %12s %12s\n",
		"Namespace", "Total Cost", "Pod Count", "CPU Cost", "Memory Cost")
	fmt.Fprintf(r.writer, "%s\n", strings.Repeat("-", 82))

	// Sort namespaces by cost
	sort.Slice(namespaceCosts, func(i, j int) bool {
		return namespaceCosts[i].TotalCost > namespaceCosts[j].TotalCost
	})

	for _, ns := range namespaceCosts {
		fmt.Fprintf(r.writer, "%-30s %12s %10d %12s %12s\n",
			ns.Name,
			fmt.Sprintf("$%.2f", ns.TotalCost),
			ns.PodCount,
			fmt.Sprintf("$%.2f", ns.CPUCost),
			fmt.Sprintf("$%.2f", ns.MemoryCost),
		)
	}

	return nil
}
