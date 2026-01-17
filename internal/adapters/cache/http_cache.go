package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	ports "github.com/ochestra-tech/k8s-monitor/internal/ports/cache"
)

const defaultCacheServiceURL = "http://localhost:8083"

const cacheRequestTimeout = 5 * time.Second

type httpCache struct {
	baseURL string
	client  *http.Client
}

type cacheSetRequest struct {
	Key        string `json:"key"`
	Value      string `json:"value"`
	TTLSeconds int64  `json:"ttl_seconds"`
}

type cacheGetResponse struct {
	Value string `json:"value"`
	Found bool   `json:"found"`
}

func NewFromEnv() ports.Store {
	cacheURL := strings.TrimSpace(os.Getenv("K8S_MONITOR_CACHE_URL"))
	if cacheURL == "" {
		cacheURL = defaultCacheServiceURL
	}

	cacheURL = strings.TrimRight(cacheURL, "/")
	return &httpCache{
		baseURL: cacheURL,
		client:  &http.Client{Timeout: cacheRequestTimeout},
	}
}

func (c *httpCache) Get(ctx context.Context, key string) (string, bool, error) {
	if c == nil {
		return "", false, nil
	}

	endpoint := fmt.Sprintf("%s/cache/%s", c.baseURL, url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", false, fmt.Errorf("cache get status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload cacheGetResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", false, err
	}
	if !payload.Found || payload.Value == "" {
		return "", false, nil
	}
	return payload.Value, true, nil
}

func (c *httpCache) Set(ctx context.Context, key string, value string, ttl time.Duration) error {
	if c == nil {
		return nil
	}

	body := cacheSetRequest{
		Key:        key,
		Value:      value,
		TTLSeconds: int64(ttl.Seconds()),
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s/cache", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cache set status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}
	return nil
}
