package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPClient is a reusable base HTTP client for exchange REST APIs.
// Each venue adapter embeds this and adds venue-specific signing/headers.
type HTTPClient struct {
	Client  *http.Client
	BaseURL string
}

// NewHTTPClient creates an HTTPClient with sensible defaults.
func NewHTTPClient(baseURL string, timeout time.Duration) *HTTPClient {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &HTTPClient{
		Client:  &http.Client{Timeout: timeout},
		BaseURL: strings.TrimRight(baseURL, "/"),
	}
}

// DoJSON executes an HTTP request and decodes the JSON response into dst.
// If dst is nil, the response body is discarded.
func (c *HTTPClient) DoJSON(ctx context.Context, method, path string, body io.Reader, headers map[string]string, dst interface{}) error {
	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return fmt.Errorf("executing request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
			Method:     method,
			Path:       path,
		}
	}

	if dst != nil {
		if err := json.Unmarshal(respBody, dst); err != nil {
			return fmt.Errorf("decoding response from %s %s: %w", method, path, err)
		}
	}
	return nil
}

// APIError represents a non-2xx response from an exchange API.
type APIError struct {
	StatusCode int
	Body       string
	Method     string
	Path       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("exchange API error: %s %s returned %d: %s",
		e.Method, e.Path, e.StatusCode, e.Body)
}

// IsRateLimited returns true if the error is a 429 rate limit.
func (e *APIError) IsRateLimited() bool {
	return e.StatusCode == 429
}

// IsTemporary returns true if the error is likely transient (5xx, 429).
func (e *APIError) IsTemporary() bool {
	return e.StatusCode == 429 || e.StatusCode >= 500
}
