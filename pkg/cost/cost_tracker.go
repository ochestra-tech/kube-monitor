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
	CPU          float64 // per core hour
	Memory       float64 // per GB hour
	Storage      float64 // per GB hour
	Network      float64 // per GB
	TotalPerHour float64 // total per hour for the instance
	GPUPricing   map[string]float64
}

// listAllNodes returns all nodes in the cluster using paginated List calls.
func listAllNodes(ctx context.Context, clientset *kubernetes.Clientset) ([]v1.Node, error) {
	const pageSize = 500
	var all []v1.Node
	continueToken := ""
	for {
		list, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			Limit:    pageSize,
			Continue: continueToken,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list nodes: %w", err)
		}
		all = append(all, list.Items...)
		if list.Continue == "" {
			break
		}
		continueToken = list.Continue
	}
	return all, nil
}

// listAllPods returns all pods (across all namespaces) using paginated List calls.
func listAllPods(ctx context.Context, clientset *kubernetes.Clientset, opts metav1.ListOptions) ([]v1.Pod, error) {
	const pageSize = 500
	var all []v1.Pod
	opts.Limit = pageSize
	for {
		list, err := clientset.CoreV1().Pods("").List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to list pods: %w", err)
		}
		all = append(all, list.Items...)
		if list.Continue == "" {
			break
		}
		opts.Continue = list.Continue
	}
	return all, nil
}

// GetNodeCosts calculates costs for all nodes in the cluster
func GetNodeCosts(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	metricsClient *metricsv.Clientset,
	pricing map[string]ResourcePricing,
) ([]NodeCostData, error) {
	nodeList, err := listAllNodes(ctx, clientset)
	if err != nil {
		return nil, err
	}

	// Pre-fetch all pods once and index by node name to avoid N+1 API calls.
	allPods, err := listAllPods(ctx, clientset, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	podsByNode := make(map[string][]v1.Pod, len(nodeList))
	for _, pod := range allPods {
		podsByNode[pod.Spec.NodeName] = append(podsByNode[pod.Spec.NodeName], pod)
	}

	results := make([]NodeCostData, 0, len(nodeList))

	for _, node := range nodeList {
		nodeData := NodeCostData{
			Name:         node.Name,
			InstanceType: node.Labels["node.kubernetes.io/instance-type"],
			Region:       node.Labels["topology.kubernetes.io/region"],
			Labels:       node.Labels,
		}

		cpuCapacity := float64(node.Status.Capacity.Cpu().Value())
		memCapacity := float64(node.Status.Capacity.Memory().Value()) / (1024 * 1024 * 1024)

		resourcePricing := pricing["default"]
		if p, ok := pricing[nodeData.InstanceType]; ok {
			resourcePricing = p
		}

		if resourcePricing.TotalPerHour > 0 {
			nodeData.TotalCost = resourcePricing.TotalPerHour
			if resourcePricing.CPU > 0 || resourcePricing.Memory > 0 || resourcePricing.Storage > 0 {
				// CPU/Memory/Storage here are per-unit rates (dollars per
				// vCPU-hour / GB-hour), not node totals -- must be scaled
				// by this node's actual capacity, same as the static-price
				// branch below. Assigning them directly previously made
				// every node of a given instance type show an identical
				// CPU/Memory cost regardless of its real capacity.
				nodeData.CPUCost = cpuCapacity * resourcePricing.CPU
				nodeData.MemoryCost = memCapacity * resourcePricing.Memory
				var storageCapacity float64
				for range node.Status.VolumesAttached {
					storageCapacity += 100 // Assume 100 GB per attached volume
				}
				nodeData.StorageCost = storageCapacity * resourcePricing.Storage
			} else {
				nodeData.CPUCost = resourcePricing.TotalPerHour
			}
		} else {
			nodeData.CPUCost = cpuCapacity * resourcePricing.CPU
			nodeData.MemoryCost = memCapacity * resourcePricing.Memory

			var storageCapacity float64
			for range node.Status.VolumesAttached {
				storageCapacity += 100 // Assume 100 GB per attached volume
			}
			nodeData.StorageCost = storageCapacity * resourcePricing.Storage
			nodeData.TotalCost = nodeData.CPUCost + nodeData.MemoryCost + nodeData.StorageCost
		}

		// Compute pod count and utilization from the pre-fetched pod index.
		nodePods := podsByNode[node.Name]
		nodeData.PodCount = len(nodePods)

		totalCPURequests := 0.0
		totalMemRequests := 0.0
		for _, pod := range nodePods {
			for _, container := range pod.Spec.Containers {
				if container.Resources.Requests != nil {
					totalCPURequests += float64(container.Resources.Requests.Cpu().MilliValue()) / 1000
					totalMemRequests += float64(container.Resources.Requests.Memory().Value()) / (1024 * 1024 * 1024)
				}
			}
		}
		if cpuCapacity > 0 && memCapacity > 0 {
			nodeData.Utilization = ((totalCPURequests/cpuCapacity)+(totalMemRequests/memCapacity)) / 2 * 100
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
	pods, err := listAllPods(ctx, clientset, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	// Pre-fetch all nodes once and index by name to avoid one Get per pod.
	nodeList, err := listAllNodes(ctx, clientset)
	if err != nil {
		return nil, err
	}
	nodeByName := make(map[string]v1.Node, len(nodeList))
	for _, n := range nodeList {
		nodeByName[n.Name] = n
	}

	results := make([]PodCostData, 0, len(pods))

	for _, pod := range pods {
		if pod.Status.Phase != v1.PodRunning {
			continue
		}

		podData := PodCostData{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Labels:    pod.Labels,
			QoSClass:  string(pod.Status.QOSClass),
		}

		// Resolve pricing from the pre-fetched node map.
		node, nodeKnown := nodeByName[pod.Spec.NodeName]
		if !nodeKnown {
			log.Printf("node %s not found for pod %s/%s; using default pricing", pod.Spec.NodeName, pod.Namespace, pod.Name)
		}
		instanceType := node.Labels["node.kubernetes.io/instance-type"]
		resourcePricing := pricing["default"]
		if p, ok := pricing[instanceType]; ok {
			resourcePricing = p
		}

		totalCPURequests := 0.0
		totalMemRequests := 0.0
		totalStorage := 0.0

		for _, container := range pod.Spec.Containers {
			if container.Resources.Requests != nil {
				totalCPURequests += float64(container.Resources.Requests.Cpu().MilliValue()) / 1000
				totalMemRequests += float64(container.Resources.Requests.Memory().Value()) / (1024 * 1024 * 1024)
			}
		}

		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil {
				totalStorage += 10 // Assume 10 GB per PVC
			}
		}

		podData.CPUCost = totalCPURequests * resourcePricing.CPU
		podData.MemoryCost = totalMemRequests * resourcePricing.Memory
		podData.StorageCost = totalStorage * resourcePricing.Storage
		podData.TotalCost = podData.CPUCost + podData.MemoryCost + podData.StorageCost
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
