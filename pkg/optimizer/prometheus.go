package optimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type promSample struct {
	Labels map[string]string
	Value  float64
}

type promAPIResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

type PrometheusClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewPrometheusClient(baseURL string) *PrometheusClient {
	return &PrometheusClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (c *PrometheusClient) QueryVector(ctx context.Context, promQL string) ([]promSample, error) {
	if c == nil || c.baseURL == "" {
		return nil, fmt.Errorf("prometheus base URL is empty")
	}

	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid prometheus url: %w", err)
	}
	u.Path = "/api/v1/query"
	q := u.Query()
	q.Set("query", promQL)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("prometheus query failed: http %d", resp.StatusCode)
	}

	var payload promAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}
	if payload.Status != "success" {
		if payload.Error != "" {
			return nil, fmt.Errorf("prometheus error (%s): %s", payload.ErrorType, payload.Error)
		}
		return nil, fmt.Errorf("prometheus query did not succeed")
	}
	if payload.Data.ResultType != "vector" {
		return nil, fmt.Errorf("unexpected prometheus resultType: %s", payload.Data.ResultType)
	}

	out := make([]promSample, 0, len(payload.Data.Result))
	for _, r := range payload.Data.Result {
		if len(r.Value) != 2 {
			continue
		}
		valueStr, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			continue
		}
		out = append(out, promSample{Labels: r.Metric, Value: f})
	}

	return out, nil
}
