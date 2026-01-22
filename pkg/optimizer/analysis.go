package optimizer

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/ochestra-tech/k8s-monitor/pkg/cost"
)

type containerUsage struct {
	cpuMilli int64
	memBytes int64
}

func normalizeOptions(opts Options) Options {
	if opts.IdleCPUPercent <= 0 {
		opts.IdleCPUPercent = 5
	}
	if opts.IdleMemoryPercent <= 0 {
		opts.IdleMemoryPercent = 5
	}
	if opts.OverprovisionedFactor <= 0 {
		opts.OverprovisionedFactor = 2.0
	}
	if opts.HeadroomFactor <= 0 {
		opts.HeadroomFactor = 1.2
	}
	if opts.NetworkIdleBytesPerSec <= 0 {
		opts.NetworkIdleBytesPerSec = 1024 // 1KB/s
	}
	if opts.StorageLowUtilPercent <= 0 {
		opts.StorageLowUtilPercent = 20
	}
	if opts.View == "" {
		opts.View = ViewFull
	}
	// Default to including all signals.
	if !opts.IncludeIdle && !opts.IncludeOverprovisioned && !opts.IncludeCleanup {
		opts.IncludeIdle = true
		opts.IncludeOverprovisioned = true
		opts.IncludeCleanup = true
		opts.DryRunCleanup = true
	}
	return opts
}

func hoursPerMonth() float64 {
	// Average month length ~ 365/12 days.
	return 365.0 / 12.0 * 24.0
}

func priceDefault(pricing map[string]cost.ResourcePricing) cost.ResourcePricing {
	if pricing == nil {
		return cost.ResourcePricing{}
	}
	if p, ok := pricing["default"]; ok {
		return p
	}
	return cost.ResourcePricing{}
}

func quantityMilli(q resource.Quantity) int64 {
	return q.MilliValue()
}

func quantityBytes(q resource.Quantity) int64 {
	return q.Value()
}

func safeString(s string) string {
	return strings.TrimSpace(s)
}

func isIdlePercent(request int64, usage int64, idlePercent float64) bool {
	if request <= 0 {
		// If there is no request set, we cannot compute % of request.
		// Treat as not-idle and rely on absolute usage in higher-level heuristics.
		return false
	}
	threshold := float64(request) * (idlePercent / 100.0)
	return float64(usage) <= threshold
}

func wasteForOverprovisioned(request int64, usage int64, overFactor float64, headroom float64) int64 {
	if request <= 0 || usage <= 0 {
		return 0
	}
	if float64(request) <= float64(usage)*overFactor {
		return 0
	}
	suggested := int64(math.Ceil(float64(usage) * headroom))
	waste := request - suggested
	if waste < 0 {
		return 0
	}
	return waste
}

func (o *ResourceOptimizer) GenerateOptimizationReport(
	ctx context.Context,
	pricing map[string]cost.ResourcePricing,
	opts Options,
) (*OptimizationReport, error) {
	opts = normalizeOptions(opts)

	report := &OptimizationReport{
		GeneratedAt: time.Now().UTC(),
		Options:     opts,
		Details:     make([]Detail, 0),
	}

	pods, err := o.clientset.CoreV1().Pods(opts.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	nsSet := make(map[string]struct{})
	for _, pod := range pods.Items {
		nsSet[pod.Namespace] = struct{}{}
	}

	namespaces := make([]string, 0, len(nsSet))
	for ns := range nsSet {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	podUsage, err := buildPodUsageMap(ctx, o.metricsClient, namespaces)
	if err != nil {
		return nil, err
	}

	// Node-level utilization.
	nodeCapCPU, nodeCapMem, err := nodeCapacities(ctx, o.clientset)
	if err != nil {
		return nil, err
	}
	nodeUsage, err := buildNodeUsageMap(ctx, o.metricsClient)
	if err != nil {
		return nil, err
	}

	idleNodes := make(map[string]struct{})
	idleNamespaces := make(map[string]struct{})
	idlePods := make(map[string]struct{})
	idleContainers := make(map[string]struct{})
	overNodes := make(map[string]struct{})
	overNamespaces := make(map[string]struct{})
	overPods := make(map[string]struct{})
	overContainers := make(map[string]struct{})

	nsAgg := make(map[string]*agg)
	nodeAgg := make(map[string]*agg)

	p := priceDefault(pricing)
	monthlyHours := hoursPerMonth()

	for _, pod := range pods.Items {
		if opts.Node != "" && safeString(pod.Spec.NodeName) != safeString(opts.Node) {
			continue
		}
		if opts.Pod != "" && safeString(pod.Name) != safeString(opts.Pod) {
			continue
		}

		podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		usageByContainer := podUsage[podKey]

		podAllIdle := true
		podAnyOver := false

		for _, c := range pod.Spec.Containers {
			containerName := c.Name
			cu, ok := usageByContainer[containerName]
			if !ok {
				podAllIdle = false
				continue
			}

			reqCPU := int64(0)
			reqMem := int64(0)
			limCPU := int64(0)
			limMem := int64(0)
			if q, ok := c.Resources.Requests["cpu"]; ok {
				reqCPU = quantityMilli(q)
			}
			if q, ok := c.Resources.Requests["memory"]; ok {
				reqMem = quantityBytes(q)
			}
			if q, ok := c.Resources.Limits["cpu"]; ok {
				limCPU = quantityMilli(q)
			}
			if q, ok := c.Resources.Limits["memory"]; ok {
				limMem = quantityBytes(q)
			}

			cpuIdle := isIdlePercent(reqCPU, cu.cpuMilli, opts.IdleCPUPercent)
			memIdle := isIdlePercent(reqMem, cu.memBytes, opts.IdleMemoryPercent)
			containerIdle := cpuIdle && memIdle

			cpuWaste := wasteForOverprovisioned(reqCPU, cu.cpuMilli, opts.OverprovisionedFactor, opts.HeadroomFactor)
			memWaste := wasteForOverprovisioned(reqMem, cu.memBytes, opts.OverprovisionedFactor, opts.HeadroomFactor)
			containerOver := cpuWaste > 0 || memWaste > 0
			podAnyOver = podAnyOver || containerOver
			podAllIdle = podAllIdle && containerIdle

			containerKey := fmt.Sprintf("%s/%s", podKey, containerName)

			if opts.IncludeIdle && containerIdle {
				idleContainers[containerKey] = struct{}{}
				report.Details = append(report.Details, Detail{
					Category:             "idle",
					Level:                "container",
					Node:                 pod.Spec.NodeName,
					Namespace:            pod.Namespace,
					Pod:                  pod.Name,
					Container:            containerName,
					CPUUsageMillicores:   cu.cpuMilli,
					CPURequestMillicores: reqCPU,
					CPULimitMillicores:   limCPU,
					MemoryUsageBytes:     cu.memBytes,
					MemoryRequestBytes:   reqMem,
					MemoryLimitBytes:     limMem,
					Recommendation:       "Consider scaling down requests or using HPA/VPA; validate workload is truly idle.",
				})
			}

			if opts.IncludeOverprovisioned && containerOver {
				overContainers[containerKey] = struct{}{}

				cpuSavings := (float64(cpuWaste) / 1000.0) * p.CPU * monthlyHours
				memSavings := (float64(memWaste) / float64(1024*1024*1024)) * p.Memory * monthlyHours
				saving := cpuSavings + memSavings

				report.Summary.WastedCPUCoreHours += (float64(cpuWaste) / 1000.0) * monthlyHours
				report.Summary.WastedMemoryGBHours += (float64(memWaste) / float64(1024*1024*1024)) * monthlyHours
				report.Summary.PotentialMonthlySavings += saving

				report.Details = append(report.Details, Detail{
					Category:               "overprovisioned",
					Level:                  "container",
					Node:                   pod.Spec.NodeName,
					Namespace:              pod.Namespace,
					Pod:                    pod.Name,
					Container:              containerName,
					CPUUsageMillicores:     cu.cpuMilli,
					CPURequestMillicores:   reqCPU,
					CPULimitMillicores:     limCPU,
					MemoryUsageBytes:       cu.memBytes,
					MemoryRequestBytes:     reqMem,
					MemoryLimitBytes:       limMem,
					WastedCPUMillicores:    cpuWaste,
					WastedMemoryBytes:      memWaste,
					PotentialMonthlySaving: saving,
					Recommendation:         "Lower requests closer to observed usage (with headroom), or adopt VPA for automatic tuning.",
				})
			}

			// Aggregate for namespace/node rollups.
			na := nsAgg[pod.Namespace]
			if na == nil {
				na = &agg{}
				nsAgg[pod.Namespace] = na
			}
			na.add(reqCPU, reqMem, cu.cpuMilli, cu.memBytes, cpuWaste, memWaste)

			node := pod.Spec.NodeName
			if node != "" {
				no := nodeAgg[node]
				if no == nil {
					no = &agg{}
					nodeAgg[node] = no
				}
				no.add(reqCPU, reqMem, cu.cpuMilli, cu.memBytes, cpuWaste, memWaste)
			}
		}

		if opts.IncludeIdle && podAllIdle {
			idlePods[podKey] = struct{}{}
		}
		if opts.IncludeOverprovisioned && podAnyOver {
			overPods[podKey] = struct{}{}
		}
	}

	// Namespace rollups.
	for ns, a := range nsAgg {
		nsKey := ns
		idle := false
		over := false

		if opts.IncludeIdle {
			idle = isIdlePercent(a.reqCPU, a.useCPU, opts.IdleCPUPercent) && isIdlePercent(a.reqMem, a.useMem, opts.IdleMemoryPercent)
			if idle {
				idleNamespaces[nsKey] = struct{}{}
				if opts.View != ViewSummary {
					report.Details = append(report.Details, Detail{
						Category:             "idle",
						Level:                "namespace",
						Namespace:            ns,
						CPUUsageMillicores:   a.useCPU,
						CPURequestMillicores: a.reqCPU,
						MemoryUsageBytes:     a.useMem,
						MemoryRequestBytes:   a.reqMem,
						Recommendation:       "Investigate idle workloads in this namespace; consider scaling down or consolidating.",
					})
				}
			}
		}
		if opts.IncludeOverprovisioned {
			over = a.cpuWaste > 0 || a.memWaste > 0
			if over {
				overNamespaces[nsKey] = struct{}{}
				if opts.View != ViewSummary {
					saving := ((float64(a.cpuWaste) / 1000.0) * p.CPU * monthlyHours) + ((float64(a.memWaste) / float64(1024*1024*1024)) * p.Memory * monthlyHours)
					report.Details = append(report.Details, Detail{
						Category:               "overprovisioned",
						Level:                  "namespace",
						Namespace:              ns,
						CPUUsageMillicores:     a.useCPU,
						CPURequestMillicores:   a.reqCPU,
						MemoryUsageBytes:       a.useMem,
						MemoryRequestBytes:     a.reqMem,
						WastedCPUMillicores:    a.cpuWaste,
						WastedMemoryBytes:      a.memWaste,
						PotentialMonthlySaving: saving,
						Recommendation:         "Reduce aggregate requests and validate autoscaling policies; use VPA/HPA for tuning.",
					})
				}
			}
		}
		_ = idle
		_ = over
	}

	// Node rollups.
	for node, a := range nodeAgg {
		cpuCap := nodeCapCPU[node]
		memCap := nodeCapMem[node]
		u := nodeUsage[node]

		cpuUtil := 0.0
		memUtil := 0.0
		if cpuCap > 0 {
			cpuUtil = float64(u.cpuMilli) / float64(cpuCap)
		}
		if memCap > 0 {
			memUtil = float64(u.memBytes) / float64(memCap)
		}

		idle := opts.IncludeIdle && (cpuUtil*100.0 <= opts.IdleCPUPercent) && (memUtil*100.0 <= opts.IdleMemoryPercent)
		if idle {
			idleNodes[node] = struct{}{}
			if opts.View != ViewSummary {
				report.Details = append(report.Details, Detail{
					Category:             "idle",
					Level:                "node",
					Node:                 node,
					CPUUsageMillicores:   u.cpuMilli,
					CPURequestMillicores: cpuCap,
					MemoryUsageBytes:     u.memBytes,
					MemoryRequestBytes:   memCap,
					Recommendation:       "Consider consolidating workloads or scaling down node pool capacity.",
				})
			}
		}

		over := opts.IncludeOverprovisioned && (a.cpuWaste > 0 || a.memWaste > 0)
		if over {
			overNodes[node] = struct{}{}
			if opts.View != ViewSummary {
				saving := ((float64(a.cpuWaste) / 1000.0) * p.CPU * monthlyHours) + ((float64(a.memWaste) / float64(1024*1024*1024)) * p.Memory * monthlyHours)
				report.Details = append(report.Details, Detail{
					Category:               "overprovisioned",
					Level:                  "node",
					Node:                   node,
					CPUUsageMillicores:     a.useCPU,
					CPURequestMillicores:   a.reqCPU,
					MemoryUsageBytes:       a.useMem,
					MemoryRequestBytes:     a.reqMem,
					WastedCPUMillicores:    a.cpuWaste,
					WastedMemoryBytes:      a.memWaste,
					PotentialMonthlySaving: saving,
					Recommendation:         "Tune requests on workloads scheduled to this node; consider binpacking improvements.",
				})
			}
		}
	}

	// Prometheus-backed signals (network + storage).
	if o.promClient != nil {
		if opts.IncludeNetwork {
			rxSamples, err := o.promClient.QueryVector(ctx, `sum by (namespace,pod) (rate(container_network_receive_bytes_total{namespace!="",pod!=""}[5m]))`)
			if err != nil {
				return nil, fmt.Errorf("prometheus network rx query failed: %w", err)
			}
			txSamples, err := o.promClient.QueryVector(ctx, `sum by (namespace,pod) (rate(container_network_transmit_bytes_total{namespace!="",pod!=""}[5m]))`)
			if err != nil {
				return nil, fmt.Errorf("prometheus network tx query failed: %w", err)
			}

			rx := make(map[string]float64, len(rxSamples))
			for _, s := range rxSamples {
				ns := s.Labels["namespace"]
				pod := s.Labels["pod"]
				if ns == "" || pod == "" {
					continue
				}
				rx[fmt.Sprintf("%s/%s", ns, pod)] = s.Value
			}
			tx := make(map[string]float64, len(txSamples))
			for _, s := range txSamples {
				ns := s.Labels["namespace"]
				pod := s.Labels["pod"]
				if ns == "" || pod == "" {
					continue
				}
				tx[fmt.Sprintf("%s/%s", ns, pod)] = s.Value
			}

			for key, rxv := range rx {
				txv := tx[key]
				parts := strings.SplitN(key, "/", 2)
				if len(parts) != 2 {
					continue
				}
				ns, podName := parts[0], parts[1]

				if opts.Namespace != "" && safeString(ns) != safeString(opts.Namespace) {
					continue
				}
				if opts.Pod != "" && safeString(podName) != safeString(opts.Pod) {
					continue
				}

				if (rxv + txv) > opts.NetworkIdleBytesPerSec {
					continue
				}

				if opts.View != ViewSummary {
					report.Details = append(report.Details, Detail{
						Category:                   "network_idle",
						Level:                      "pod",
						Namespace:                  ns,
						Pod:                        podName,
						NetworkReceiveBytesPerSec:  rxv,
						NetworkTransmitBytesPerSec: txv,
						Recommendation:             "Pod network throughput is very low; consider scaling down replicas or consolidating if truly idle.",
					})
				}
			}
		}

		if opts.IncludeStorage {
			usedSamples, err := o.promClient.QueryVector(ctx, `sum by (namespace,persistentvolumeclaim) (kubelet_volume_stats_used_bytes{namespace!="",persistentvolumeclaim!=""})`)
			if err != nil {
				return nil, fmt.Errorf("prometheus pvc used query failed: %w", err)
			}
			capSamples, err := o.promClient.QueryVector(ctx, `sum by (namespace,persistentvolumeclaim) (kubelet_volume_stats_capacity_bytes{namespace!="",persistentvolumeclaim!=""})`)
			if err != nil {
				return nil, fmt.Errorf("prometheus pvc capacity query failed: %w", err)
			}

			used := make(map[string]float64, len(usedSamples))
			for _, s := range usedSamples {
				ns := s.Labels["namespace"]
				pvc := s.Labels["persistentvolumeclaim"]
				if ns == "" || pvc == "" {
					continue
				}
				used[fmt.Sprintf("%s/%s", ns, pvc)] = s.Value
			}
			cap := make(map[string]float64, len(capSamples))
			for _, s := range capSamples {
				ns := s.Labels["namespace"]
				pvc := s.Labels["persistentvolumeclaim"]
				if ns == "" || pvc == "" {
					continue
				}
				cap[fmt.Sprintf("%s/%s", ns, pvc)] = s.Value
			}

			for key, usedBytesF := range used {
				capBytesF := cap[key]
				if capBytesF <= 0 {
					continue
				}
				util := (usedBytesF / capBytesF) * 100.0
				if util > opts.StorageLowUtilPercent {
					continue
				}

				parts := strings.SplitN(key, "/", 2)
				if len(parts) != 2 {
					continue
				}
				ns, pvcName := parts[0], parts[1]
				if opts.Namespace != "" && safeString(ns) != safeString(opts.Namespace) {
					continue
				}

				usedBytes := int64(math.Round(usedBytesF))
				capBytes := int64(math.Round(capBytesF))
				suggested := int64(math.Ceil(float64(usedBytes) * opts.HeadroomFactor))
				wasted := capBytes - suggested
				if wasted < 0 {
					wasted = 0
				}

				saving := (float64(wasted) / float64(1024*1024*1024)) * p.Storage * monthlyHours
				if saving > 0 {
					report.Summary.PotentialMonthlySavings += saving
				}

				if opts.View != ViewSummary {
					report.Details = append(report.Details, Detail{
						Category:               "storage_overprovisioned",
						Level:                  "pvc",
						Namespace:              ns,
						PVC:                    pvcName,
						PVCUsedBytes:           usedBytes,
						PVCCapacityBytes:       capBytes,
						PVCUtilizationPercent:  util,
						PVCWastedBytes:         wasted,
						PotentialMonthlySaving: saving,
						Recommendation:         "PVC utilization is low; consider right-sizing (if your storage supports shrink) or migrate data to a smaller volume/class.",
					})
				}
			}
		}
	}

	report.Summary.IdleResources = Counts{
		Nodes:      len(idleNodes),
		Namespaces: len(idleNamespaces),
		Pods:       len(idlePods),
		Containers: len(idleContainers),
	}
	report.Summary.Overprovisioned = Counts{
		Nodes:      len(overNodes),
		Namespaces: len(overNamespaces),
		Pods:       len(overPods),
		Containers: len(overContainers),
	}

	if opts.IncludeCleanup && opts.View != ViewSummary {
		cleanup, err := CleanupUnusedResources(ctx, o.clientset, opts.Namespace, opts.DryRunCleanup)
		if err != nil {
			return nil, err
		}
		report.Cleanup = cleanup
	}

	if opts.View == ViewSummary {
		report.Details = nil
		report.Cleanup = nil
	}

	// Stable ordering for UI consumption.
	sort.Slice(report.Details, func(i, j int) bool {
		a, b := report.Details[i], report.Details[j]
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Level != b.Level {
			return a.Level < b.Level
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		if a.Pod != b.Pod {
			return a.Pod < b.Pod
		}
		if a.PVC != b.PVC {
			return a.PVC < b.PVC
		}
		return a.Container < b.Container
	})

	return report, nil
}

type agg struct {
	reqCPU   int64
	reqMem   int64
	useCPU   int64
	useMem   int64
	cpuWaste int64
	memWaste int64
}

func (a *agg) add(reqCPU, reqMem, useCPU, useMem, cpuWaste, memWaste int64) {
	a.reqCPU += reqCPU
	a.reqMem += reqMem
	a.useCPU += useCPU
	a.useMem += useMem
	a.cpuWaste += cpuWaste
	a.memWaste += memWaste
}

func buildPodUsageMap(ctx context.Context, metricsClient *metricsv.Clientset, namespaces []string) (map[string]map[string]containerUsage, error) {
	out := make(map[string]map[string]containerUsage)

	for _, ns := range namespaces {
		pm, err := metricsClient.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list pod metrics for namespace %q (is metrics-server installed?): %w", ns, err)
		}
		for _, p := range pm.Items {
			key := fmt.Sprintf("%s/%s", p.Namespace, p.Name)
			m := make(map[string]containerUsage)
			for _, c := range p.Containers {
				m[c.Name] = containerUsage{
					cpuMilli: c.Usage.Cpu().MilliValue(),
					memBytes: c.Usage.Memory().Value(),
				}
			}
			out[key] = m
		}
	}

	return out, nil
}

func buildNodeUsageMap(ctx context.Context, metricsClient *metricsv.Clientset) (map[string]containerUsage, error) {
	out := make(map[string]containerUsage)
	nm, err := metricsClient.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list node metrics (is metrics-server installed?): %w", err)
	}
	for _, n := range nm.Items {
		out[n.Name] = containerUsage{
			cpuMilli: n.Usage.Cpu().MilliValue(),
			memBytes: n.Usage.Memory().Value(),
		}
	}
	return out, nil
}

func nodeCapacities(ctx context.Context, clientset *kubernetes.Clientset) (map[string]int64, map[string]int64, error) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	cpu := make(map[string]int64, len(nodes.Items))
	mem := make(map[string]int64, len(nodes.Items))
	for _, n := range nodes.Items {
		// Capacity CPU is in cores; normalize to millicores.
		cpu[n.Name] = n.Status.Capacity.Cpu().MilliValue()
		mem[n.Name] = n.Status.Capacity.Memory().Value()
	}
	return cpu, mem, nil
}
