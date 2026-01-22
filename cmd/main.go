package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	cacheadapter "github.com/ochestra-tech/k8s-monitor/internal/adapters/cache"
	awsp "github.com/ochestra-tech/k8s-monitor/internal/adapters/pricing/aws"
	azurep "github.com/ochestra-tech/k8s-monitor/internal/adapters/pricing/azure"
	gcpp "github.com/ochestra-tech/k8s-monitor/internal/adapters/pricing/gcp"
	staticp "github.com/ochestra-tech/k8s-monitor/internal/adapters/pricing/static"
	reportingadapter "github.com/ochestra-tech/k8s-monitor/internal/adapters/reporting"
	apppricing "github.com/ochestra-tech/k8s-monitor/internal/app/pricing"
	appreporting "github.com/ochestra-tech/k8s-monitor/internal/app/reporting"
	domainpricing "github.com/ochestra-tech/k8s-monitor/internal/domain/pricing"
	portspricing "github.com/ochestra-tech/k8s-monitor/internal/ports/pricing"
	"github.com/ochestra-tech/k8s-monitor/pkg/cost"
	"github.com/ochestra-tech/k8s-monitor/pkg/optimizer"
	"github.com/ochestra-tech/k8s-monitor/pkg/reports"
)

const defaultPricingConfigPath = "configs/pricing-config.json"

var (
	reportRunDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "k8s_monitor_report_duration_seconds",
			Help:    "Duration of report generation by type and status.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"report_type", "status"},
	)

	reportRunTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8s_monitor_report_total",
			Help: "Total number of report runs by type and status.",
		},
		[]string{"report_type", "status"},
	)

	reportLastSuccessTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_monitor_report_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful report run by type.",
		},
		[]string{"report_type"},
	)

	podPhaseTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_monitor_pods_phase_total",
			Help: "Number of pods by phase across the cluster.",
		},
		[]string{"phase"},
	)

	namespacePodPhaseTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_monitor_namespace_pods_phase_total",
			Help: "Number of pods by phase for top namespaces (others aggregated).",
		},
		[]string{"namespace", "phase"},
	)

	namespacePodTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_monitor_namespace_pods_total",
			Help: "Number of pods per top namespace (others aggregated).",
		},
		[]string{"namespace"},
	)
)

func init() {
	prometheus.MustRegister(reportRunDuration)
	prometheus.MustRegister(reportRunTotal)
	prometheus.MustRegister(reportLastSuccessTimestamp)
	prometheus.MustRegister(podPhaseTotal)
	prometheus.MustRegister(namespacePodPhaseTotal)
	prometheus.MustRegister(namespacePodTotal)
}

// Config holds the application configuration
type Config struct {
	KubeConfigPath           string
	PricingConfigPath        string
	ReportFormat             reports.ReportFormat
	ReportPath               string
	CheckInterval            time.Duration
	MetricsPort              int
	MetricsReadHeaderTimeout time.Duration
	RequestTimeout           time.Duration
	ShutdownTimeout          time.Duration
	KubeQPS                  float32
	KubeBurst                int
	EnableDetailedMetrics    bool
	MetricsTopNamespaces     int
	DetailedMetricsInterval  time.Duration
	PricingDebug             bool
	PricingDebugLogPath      string
	OneShot                  bool
	ReportType               string // "health", "cost", "combined"
}

func main() {
	config := parseFlags()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clientset, metricsClient := initKubernetesClients(config.KubeConfigPath, config.KubeQPS, config.KubeBurst)
	pricing, err := resolvePricing(ctx, config.PricingConfigPath, config.PricingDebug, config.PricingDebugLogPath)
	if err != nil {
		log.Fatalf("Failed to resolve pricing: %v", err)
	}

	if config.OneShot {
		if err := runWithTimeout(ctx, config.RequestTimeout, func(runCtx context.Context) error {
			return executeReportCycle(runCtx, clientset, metricsClient, pricing, config)
		}); err != nil {
			log.Fatalf("Failed to generate report: %v", err)
		}
		if config.EnableDetailedMetrics {
			if err := updateDetailedMetrics(ctx, clientset, config.MetricsTopNamespaces); err != nil {
				log.Printf("Failed to update detailed metrics: %v", err)
			}
		}
		return
	}

	metricsServer := startMetricsServer(config, clientset, metricsClient, pricing)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
		defer cancel()
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("Metrics server shutdown error: %v", err)
		}
	}()

	ticker := time.NewTicker(config.CheckInterval)
	defer ticker.Stop()

	if err := runWithTimeout(ctx, config.RequestTimeout, func(runCtx context.Context) error {
		return executeReportCycle(runCtx, clientset, metricsClient, pricing, config)
	}); err != nil {
		log.Printf("Failed to generate initial report: %v", err)
	}
	if config.EnableDetailedMetrics {
		if err := updateDetailedMetrics(ctx, clientset, config.MetricsTopNamespaces); err != nil {
			log.Printf("Failed to update detailed metrics: %v", err)
		}
	}

	var detailedTicker *time.Ticker
	var detailedC <-chan time.Time
	if config.EnableDetailedMetrics {
		interval := config.DetailedMetricsInterval
		if interval <= 0 {
			interval = config.CheckInterval
		}
		detailedTicker = time.NewTicker(interval)
		detailedC = detailedTicker.C
		defer detailedTicker.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			log.Printf("Shutting down: %v", ctx.Err())
			return
		case <-ticker.C:
			if err := runWithTimeout(ctx, config.RequestTimeout, func(runCtx context.Context) error {
				return executeReportCycle(runCtx, clientset, metricsClient, pricing, config)
			}); err != nil {
				log.Printf("Failed to generate report: %v", err)
			}
		case <-detailedC:
			if err := updateDetailedMetrics(ctx, clientset, config.MetricsTopNamespaces); err != nil {
				log.Printf("Failed to update detailed metrics: %v", err)
			}
		}
	}
}

func parseFlags() *Config {
	config := &Config{}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to get user home directory: %v", err)
	}
	defaultKubeConfig := filepath.Join(homeDir, ".kube", "config")

	flag.StringVar(&config.KubeConfigPath, "kubeconfig", defaultKubeConfig, "Path to kubeconfig file")
	flag.StringVar(&config.PricingConfigPath, "pricing-config", defaultPricingConfigPath, "Path to pricing configuration file")
	flag.StringVar((*string)(&config.ReportFormat), "format", string(reports.FormatText), "Report format (text, json, html)")
	flag.StringVar(&config.ReportPath, "output", "", "Output file path (empty for stdout)")
	flag.DurationVar(&config.CheckInterval, "interval", 60*time.Second, "Check interval for continuous monitoring")
	flag.IntVar(&config.MetricsPort, "metrics-port", 8085, "Prometheus metrics port")
	flag.DurationVar(&config.MetricsReadHeaderTimeout, "metrics-read-header-timeout", 5*time.Second, "Read header timeout for metrics server")
	flag.DurationVar(&config.RequestTimeout, "request-timeout", 90*time.Second, "Timeout for a single report cycle")
	flag.DurationVar(&config.ShutdownTimeout, "shutdown-timeout", 10*time.Second, "Graceful shutdown timeout")
	kubeQPS := 20.0
	flag.Float64Var(&kubeQPS, "kube-qps", 20, "Kubernetes client QPS (rate limiter)")
	flag.IntVar(&config.KubeBurst, "kube-burst", 40, "Kubernetes client burst (rate limiter)")
	flag.BoolVar(&config.EnableDetailedMetrics, "enable-detailed-metrics", false, "Enable namespace/phase metrics (top namespaces only)")
	flag.IntVar(&config.MetricsTopNamespaces, "metrics-top-namespaces", 10, "Max namespaces to export metrics for (others aggregated)")
	flag.DurationVar(&config.DetailedMetricsInterval, "detailed-metrics-interval", 5*time.Minute, "Interval for detailed metrics collection")
	flag.BoolVar(&config.PricingDebug, "pricing-debug", false, "Enable debug logging for pricing providers")
	flag.StringVar(&config.PricingDebugLogPath, "pricing-debug-log", "pricing-debug.log", "File path for pricing debug logs")
	flag.BoolVar(&config.OneShot, "one-shot", false, "Run once and exit")
	flag.StringVar(&config.ReportType, "type", "combined", "Report type (health, cost, combined)")

	flag.Parse()
	config.KubeQPS = float32(kubeQPS)
	return config
}

func startMetricsServer(
	config *Config,
	clientset *kubernetes.Clientset,
	metricsClient *metricsv.Clientset,
	pricing map[string]cost.ResourcePricing,
) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	mux.Handle("/api/health", withCORS(reportHandler("health", clientset, metricsClient, pricing, config.RequestTimeout)))
	mux.Handle("/api/cost", withCORS(reportHandler("cost", clientset, metricsClient, pricing, config.RequestTimeout)))
	mux.Handle("/api/combined", withCORS(reportHandler("combined", clientset, metricsClient, pricing, config.RequestTimeout)))
	mux.Handle("/api/optimizer", withCORS(optimizerHandler(clientset, metricsClient, pricing, config.RequestTimeout)))

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", config.MetricsPort),
		Handler:           mux,
		ReadHeaderTimeout: config.MetricsReadHeaderTimeout,
	}

	go func() {
		log.Printf("Starting metrics server on port %d", config.MetricsPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start metrics server: %v", err)
		}
	}()

	return server
}

func optimizerHandler(
	clientset *kubernetes.Clientset,
	metricsClient *metricsv.Clientset,
	pricing map[string]cost.ResourcePricing,
	timeout time.Duration,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx := r.Context()
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		q := r.URL.Query()
		opts := optimizer.Options{
			Namespace: q.Get("namespace"),
			Node:      q.Get("node"),
			Pod:       q.Get("pod"),
			View:      optimizer.ReportView(q.Get("view")),
		}

		// Toggles (default true)
		opts.IncludeIdle = q.Get("includeIdle") != "false"
		opts.IncludeOverprovisioned = q.Get("includeOverprovisioned") != "false"
		opts.IncludeCleanup = q.Get("includeCleanup") != "false"
		opts.DryRunCleanup = q.Get("dryRunCleanup") != "false"
		opts.IncludeNetwork = q.Get("includeNetwork") != "false"
		opts.IncludeStorage = q.Get("includeStorage") != "false"

		// Thresholds
		if v := q.Get("idleCpuPercent"); v != "" {
			if parsed, err := parseFloat(v); err == nil {
				opts.IdleCPUPercent = parsed
			}
		}
		if v := q.Get("idleMemoryPercent"); v != "" {
			if parsed, err := parseFloat(v); err == nil {
				opts.IdleMemoryPercent = parsed
			}
		}
		if v := q.Get("overprovisionedFactor"); v != "" {
			if parsed, err := parseFloat(v); err == nil {
				opts.OverprovisionedFactor = parsed
			}
		}
		if v := q.Get("headroomFactor"); v != "" {
			if parsed, err := parseFloat(v); err == nil {
				opts.HeadroomFactor = parsed
			}
		}
		if v := q.Get("networkIdleBytesPerSec"); v != "" {
			if parsed, err := parseFloat(v); err == nil {
				opts.NetworkIdleBytesPerSec = parsed
			}
		}
		if v := q.Get("storageLowUtilPercent"); v != "" {
			if parsed, err := parseFloat(v); err == nil {
				opts.StorageLowUtilPercent = parsed
			}
		}

		opt := optimizer.NewResourceOptimizerWithPrometheus(clientset, metricsClient, os.Getenv("PROMETHEUS_URL"))
		report, err := opt.GenerateOptimizationReport(ctx, pricing, opts)
		if err != nil {
			status := http.StatusInternalServerError
			payload := map[string]string{
				"error":   "failed to generate optimizer report",
				"details": err.Error(),
			}
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
				payload["suggestion"] = "Increase --request-timeout or filter by namespace/node to reduce the amount of data scanned."
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(payload)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(report); err != nil {
			log.Printf("Failed to write optimizer report response: %v", err)
		}
	}
}

func parseFloat(s string) (float64, error) {
	// Keep dependencies minimal; use fmt for parsing.
	var v float64
	_, err := fmt.Sscanf(s, "%f", &v)
	return v, err
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func reportHandler(
	reportType string,
	clientset *kubernetes.Clientset,
	metricsClient *metricsv.Clientset,
	pricing map[string]cost.ResourcePricing,
	timeout time.Duration,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx := r.Context()
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		var buf bytes.Buffer
		generator := reportingadapter.NewGenerator(clientset, metricsClient, reports.FormatJSON, &buf)
		service := appreporting.NewService(generator)
		if err := service.Generate(ctx, reportType, pricing); err != nil {
			status := http.StatusInternalServerError
			payload := map[string]string{
				"error":   fmt.Sprintf("failed to generate %s report", reportType),
				"details": err.Error(),
			}
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
				payload["suggestion"] = "Increase --request-timeout or reduce report scope to avoid API throttling."
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if encodeErr := json.NewEncoder(w).Encode(payload); encodeErr != nil {
				log.Printf("Failed to write %s error response: %v", reportType, encodeErr)
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(buf.Bytes()); err != nil {
			log.Printf("Failed to write %s report response: %v", reportType, err)
		}
	}
}

func runWithTimeout(parentCtx context.Context, timeout time.Duration, run func(context.Context) error) error {
	if timeout <= 0 {
		return run(parentCtx)
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()
	return run(ctx)
}

func initKubernetesClients(kubeConfigPath string, qps float32, burst int) (*kubernetes.Clientset, *metricsv.Clientset) {
	var config *rest.Config
	var err error

	config, err = rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfigPath)
		if err != nil {
			log.Fatalf("Failed to create Kubernetes config: %v", err)
		}
	}

	if qps > 0 {
		config.QPS = qps
	}
	if burst > 0 {
		config.Burst = burst
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	metricsClient, err := metricsv.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Metrics client: %v", err)
	}

	return clientset, metricsClient
}

func resolvePricing(ctx context.Context, configPath string, debug bool, debugLogPath string) (map[string]cost.ResourcePricing, error) {
	configData, err := apppricing.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}
	configData = applyProviderEnvOverrides(configData)

	var debugLogger *log.Logger
	var debugFile *os.File
	if debug {
		path := strings.TrimSpace(debugLogPath)
		if path == "" {
			path = "pricing-debug.log"
		}
		debugFile, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return nil, fmt.Errorf("open pricing debug log: %w", err)
		}
		debugLogger = log.New(debugFile, "", log.LstdFlags|log.LUTC)
	}
	if debugFile != nil {
		defer debugFile.Close()
	}

	providers := make([]portspricing.Provider, 0)
	staticProvider := staticp.New(configData.InstanceTypes, configData.Providers.AWS.Region, configData.Providers.AWS.Currency)
	providers = append(providers, staticProvider)

	azureProvider := azurep.New(nil, debug, debugLogger)
	providers = append(providers, azureProvider)

	if configData.Providers.GCP.APIKey != "" {
		gcpProvider := gcpp.New(nil, configData.Providers.GCP.APIKey, debug, debugLogger)
		providers = append(providers, gcpProvider)
	}

	awsProvider, err := awsp.New(ctx, debug, debugLogger)
	if err == nil {
		providers = append(providers, awsProvider)
	}

	cacheStore := cacheadapter.NewFromEnv()
	service := apppricing.NewService(providers, cacheStore)
	return service.Resolve(ctx, configData)
}

func applyProviderEnvOverrides(cfg domainpricing.Config) domainpricing.Config {
	if value := os.Getenv("K8S_MONITOR_GCP_API_KEY"); value != "" {
		cfg.Providers.GCP.APIKey = value
	}
	return cfg
}

func generateReport(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	metricsClient *metricsv.Clientset,
	pricing map[string]cost.ResourcePricing,
	config *Config,
) error {
	var output io.Writer = os.Stdout
	if config.ReportPath != "" {
		file, err := os.Create(config.ReportPath)
		if err != nil {
			return fmt.Errorf("failed to create output file: %w", err)
		}
		defer file.Close()
		output = file
	}

	generator := reportingadapter.NewGenerator(clientset, metricsClient, config.ReportFormat, output)
	service := appreporting.NewService(generator)
	return service.Generate(ctx, config.ReportType, pricing)
}

func executeReportCycle(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	metricsClient *metricsv.Clientset,
	pricing map[string]cost.ResourcePricing,
	config *Config,
) error {
	start := time.Now()
	status := "success"
	err := generateReport(ctx, clientset, metricsClient, pricing, config)
	if err != nil {
		status = "error"
	}

	reportRunDuration.WithLabelValues(config.ReportType, status).Observe(time.Since(start).Seconds())
	reportRunTotal.WithLabelValues(config.ReportType, status).Inc()
	if status == "success" {
		reportLastSuccessTimestamp.WithLabelValues(config.ReportType).Set(float64(time.Now().Unix()))
	}

	return err
}

func updateDetailedMetrics(ctx context.Context, clientset *kubernetes.Clientset, topNamespaces int) error {
	if topNamespaces <= 0 {
		topNamespaces = 10
	}

	pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pods for metrics: %w", err)
	}

	phaseTotals := map[string]float64{}
	perNamespace := map[string]map[string]float64{}
	perNamespaceTotal := map[string]float64{}

	for _, pod := range pods.Items {
		phase := string(pod.Status.Phase)
		if phase == "" {
			phase = "Unknown"
		}
		phaseTotals[phase]++

		ns := pod.Namespace
		if _, ok := perNamespace[ns]; !ok {
			perNamespace[ns] = map[string]float64{}
		}
		perNamespace[ns][phase]++
		perNamespaceTotal[ns]++
	}

	for phase, count := range phaseTotals {
		podPhaseTotal.WithLabelValues(phase).Set(count)
	}

	topList := topNamespaceList(perNamespaceTotal, topNamespaces)
	allowed := map[string]struct{}{}
	for _, ns := range topList {
		allowed[ns] = struct{}{}
	}

	// Reset namespace vectors to avoid stale series from previous intervals
	namespacePodPhaseTotal.Reset()
	namespacePodTotal.Reset()

	otherPhaseTotals := map[string]float64{}
	var otherTotal float64

	for ns, phaseMap := range perNamespace {
		if _, ok := allowed[ns]; ok {
			for phase, count := range phaseMap {
				namespacePodPhaseTotal.WithLabelValues(ns, phase).Set(count)
			}
			namespacePodTotal.WithLabelValues(ns).Set(perNamespaceTotal[ns])
			continue
		}
		for phase, count := range phaseMap {
			otherPhaseTotals[phase] += count
		}
		otherTotal += perNamespaceTotal[ns]
	}

	if otherTotal > 0 {
		for phase, count := range otherPhaseTotals {
			namespacePodPhaseTotal.WithLabelValues("other", phase).Set(count)
		}
		namespacePodTotal.WithLabelValues("other").Set(otherTotal)
	}

	return nil
}

func topNamespaceList(counts map[string]float64, limit int) []string {
	type pair struct {
		name  string
		count float64
	}
	items := make([]pair, 0, len(counts))
	for name, count := range counts {
		items = append(items, pair{name: name, count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].count > items[j].count
	})
	if limit > len(items) {
		limit = len(items)
	}
	result := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		result = append(result, items[i].name)
	}
	return result
}
