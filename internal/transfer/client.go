package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client is an HTTP client for the transfer-policy service.
// It calls POST /check before every withdrawal to enforce policy.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a transfer-policy client. baseURL is the full URL
// of the service, e.g. "http://transfer-policy:8085".
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// CheckResult is the outcome of a transfer policy check.
type CheckResult struct {
	RequestID        string `json:"request_id"`
	Status           Status `json:"status"`
	DenialCode       string `json:"denial_code,omitempty"`
	Reason           string `json:"reason"`
	RequiresApproval bool   `json:"requires_approval"`
}

// Check validates a withdrawal against the transfer-policy service.
// Returns an error if the policy denies the transfer or if the service
// is unreachable (fail-closed: unreachable = deny).
func (c *Client) Check(ctx context.Context, req Request) (*CheckResult, error) {
	if req.RequestedAt.IsZero() {
		req.RequestedAt = time.Now().UTC()
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/check", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("transfer-policy unreachable (fail-closed): %w", err)
	}
	defer resp.Body.Close()

	var result CheckResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &result, nil
}

// MustApprove calls Check and returns an error if the transfer is not approved.
// This is the convenience method treasury and execution should use.
func (c *Client) MustApprove(ctx context.Context, req Request) error {
	result, err := c.Check(ctx, req)
	if err != nil {
		return fmt.Errorf("policy check failed (fail-closed): %w", err)
	}
	if result.Status == StatusDenied {
		return fmt.Errorf("transfer denied: %s — %s", result.DenialCode, result.Reason)
	}
	if result.Status == StatusPending {
		return fmt.Errorf("transfer requires manual approval (request_id=%s)", result.RequestID)
	}
	return nil
}
