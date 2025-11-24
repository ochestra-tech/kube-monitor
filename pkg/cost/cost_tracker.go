package cost

import (
	"context"
	"fmt"
	"log"
	"sort"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

// PodCostData represents cost information for a pod
type PodCostData struct {
	Namespace   string
	Name        string
	CPUCost     float64
	MemoryCost  float64
	StorageCost float64
	NetworkCost float64
	TotalCost   float64
	Labels      map[string]string
	QoSClass    string
	Efficiency  float64
}

// NodeCostData represents cost information for a node
type NodeCostData struct {
	Name         string
	InstanceType string
	Region       string
	CPUCost      float64
	MemoryCost   float64
	StorageCost  float64
	TotalCost    float64
	Utilization  float64
	PodCount     int
	Labels       map[string]string
}

// NamespaceCostData represents cost information for a namespace
type NamespaceCostData struct {
	Name        string
	CPUCost     float64
	MemoryCost  float64
	StorageCost float64
	TotalCost   float64
	PodCount    int
}

// ResourcePricing contains pricing information for a resource
type ResourcePricing struct {
	CPU        float64 // per core hour
	Memory     float64 // per GB hour
	Storage    float64 // per GB hour
	Network    float64 // per GB
	GPUPricing map[string]float64
}

// GetNodeCosts calculates costs for all nodes in the cluster
func GetNodeCosts(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	metricsClient *metricsv.Clientset,
	pricing map[string]ResourcePricing,
) ([]NodeCostData, error) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	results := make([]NodeCostData, 0, len(nodes.Items))

	for _, node := range nodes.Items {
		// Get node metadata
		nodeData := NodeCostData{
			Name:         node.Name,
			InstanceType: node.Labels["node.kubernetes.io/instance-type"],
			Region:       node.Labels["topology.kubernetes.io/region"],
			Labels:       node.Labels,
		}

		// Determine which pricing to use based on instance type
		resourcePricing := pricing["default"]
		if p, ok := pricing[nodeData.InstanceType]; ok {
			resourcePricing = p
		}

		// Calculate CPU cost
		cpuCapacity := float64(node.Status.Capacity.Cpu().Value())
		nodeData.CPUCost = cpuCapacity * resourcePricing.CPU

		// Calculate memory cost (convert to GB)
		memCapacity := float64(node.Status.Capacity.Memory().Value()) / (1024 * 1024 * 1024)
		nodeData.MemoryCost = memCapacity * resourcePricing.Memory

		// Calculate storage cost
		var storageCapacity float64
		for _, fs := range node.Status.VolumesAttached {
			// In a real implementation, we would get actual PV sizes
			// This is a simplified version
			storageCapacity += 100 // Assume 100GB per attached volume
		}
		nodeData.StorageCost = storageCapacity * resourcePricing.Storage

		// Calculate total cost
		nodeData.TotalCost = nodeData.CPUCost + nodeData.MemoryCost + nodeData.StorageCost

		// Calculate utilization and pod count (simplified)
		pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name),
		})
		if err != nil {
			log.Printf("Failed to list pods for node %s: %v", node.Name, err)
		} else {
			nodeData.PodCount = len(pods.Items)

			// Calculate utilization based on total requests vs capacity
			totalCPURequests := 0.0
			totalMemRequests := 0.0

			for _, pod := range pods.Items {
				for _, container := range pod.Spec.Containers {
					if container.Resources.Requests != nil {
						cpuReq := float64(container.Resources.Requests.Cpu().MilliValue()) / 1000
						memReq := float64(container.Resources.Requests.Memory().Value()) / (1024 * 1024 * 1024)

						totalCPURequests += cpuReq
						totalMemRequests += memReq
					}
				}
			}

			cpuUtilization := totalCPURequests / cpuCapacity
			memUtilization := totalMemRequests / memCapacity

			// Average of CPU and memory utilization
			nodeData.Utilization = (cpuUtilization + memUtilization) / 2 * 100
		}

		results = append(results, nodeData)
	}

	return results, nil
}

// GetPodCosts calculates costs for all pods in the cluster
func GetPodCosts(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	metricsClient *metricsv.Clientset,
	pricing map[string]ResourcePricing,
) ([]PodCostData, error) {
	pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	results := make([]PodCostData, 0, len(pods.Items))

	for _, pod := range pods.Items {
		// Skip pods that are not running
		if pod.Status.Phase != v1.PodRunning {
			continue
		}

		podData := PodCostData{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Labels:    pod.Labels,
			QoSClass:  string(pod.Status.QOSClass),
		}

		// Get node that pod is running on
		node, err := clientset.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		if err != nil {
			log.Printf("Failed to get node %s for pod %s/%s: %v", pod.Spec.NodeName, pod.Namespace, pod.Name, err)
			continue
		}

		// Determine which pricing to use based on node's instance type
		instanceType := node.Labels["node.kubernetes.io/instance-type"]
		resourcePricing := pricing["default"]
		if p, ok := pricing[instanceType]; ok {
			resourcePricing = p
		}

		// Calculate pod resource costs
		totalCPURequests := 0.0
		totalCPULimits := 0.0
		totalMemRequests := 0.0
		totalMemLimits := 0.0
		totalStorage := 0.0

		for _, container := range pod.Spec.Containers {
			// CPU requests and limits
			if container.Resources.Requests != nil {
				if cpu, ok := container.Resources.Requests.Cpu().AsInt64(); ok {
					totalCPURequests += float64(cpu)
				} else {
					cpuMilliValue := container.Resources.Requests.Cpu().MilliValue()
					totalCPURequests += float64(cpuMilliValue) / 1000
				}
			}

			if container.Resources.Limits != nil {
				if cpu, ok := container.Resources.Limits.Cpu().AsInt64(); ok {
					totalCPULimits += float64(cpu)
				} else {
					cpuMilliValue := container.Resources.Limits.Cpu().MilliValue()
					totalCPULimits += float64(cpuMilliValue) / 1000
				}
			}

			// Memory requests and limits (convert to GB)
			if container.Resources.Requests != nil {
				totalMemRequests += float64(container.Resources.Requests.Memory().Value()) / (1024 * 1024 * 1024)
			}

			if container.Resources.Limits != nil {
				totalMemLimits += float64(container.Resources.Limits.Memory().Value()) / (1024 * 1024 * 1024)
			}
		}

		// Calculate storage costs for volumes
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil {
				// In a real implementation, we would fetch the PVC and get actual size
				// This is simplified
				totalStorage += 10 // Assume 10GB per PVC
			}
		}

		// Calculate costs
		podData.CPUCost = totalCPURequests * resourcePricing.CPU
		podData.MemoryCost = totalMemRequests * resourcePricing.Memory
		podData.StorageCost = totalStorage * resourcePricing.Storage
		// Network cost would require metrics data
		podData.NetworkCost = 0

		// Calculate total cost
		podData.TotalCost = podData.CPUCost + podData.MemoryCost + podData.StorageCost + podData.NetworkCost

		// Calculate efficiency (ratio of usage to requests)
		podData.Efficiency = 0.8 // Placeholder

		results = append(results, podData)
	}

	return results, nil
}

// GetNamespaceCosts calculates costs aggregated by namespace
func GetNamespaceCosts(podCosts []PodCostData) []NamespaceCostData {
	namespaceMap := make(map[string]NamespaceCostData)

	for _, pod := range podCosts {
		nsData, exists := namespaceMap[pod.Namespace]
		if !exists {
			nsData = NamespaceCostData{
				Name: pod.Namespace,
			}
		}

		nsData.CPUCost += pod.CPUCost
		nsData.MemoryCost += pod.MemoryCost
		nsData.StorageCost += pod.StorageCost
		nsData.TotalCost += pod.TotalCost
		nsData.PodCount++

		namespaceMap[pod.Namespace] = nsData
	}

	// Convert map to slice
	result := make([]NamespaceCostData, 0, len(namespaceMap))
	for _, data := range namespaceMap {
		result = append(result, data)
	}

	// Sort by total cost (descending)
	sort.Slice(result, func(i, j int) bool {
		return result[i].TotalCost > result[j].TotalCost
	})

	return result
}
