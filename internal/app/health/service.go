package health

import (
	"context"

	ports "github.com/ochestra-tech/k8s-monitor/internal/ports/health"
	"github.com/ochestra-tech/k8s-monitor/pkg/health"
)

// Service orchestrates health checks.
type Service struct {
	checker ports.Checker
}

func NewService(checker ports.Checker) *Service {
	return &Service{checker: checker}
}

func (s *Service) Check(ctx context.Context) (*health.ClusterHealth, error) {
	return s.checker.Check(ctx)
}
