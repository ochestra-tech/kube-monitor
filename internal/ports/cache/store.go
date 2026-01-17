package cache

import (
	"context"
	"time"
)

// Store defines cache operations for cross-cutting caching concerns.
// Values are stored as raw strings to avoid domain coupling.
type Store interface {
	Get(ctx context.Context, key string) (value string, found bool, err error)
	Set(ctx context.Context, key string, value string, ttl time.Duration) error
}
