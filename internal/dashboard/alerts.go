package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

// WebhookPayload is the JSON body POSTed to the configured webhook URL.
type WebhookPayload struct {
	Source   string `json:"source"`
	Event    string `json:"event"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	TsMs     int64  `json:"ts_ms"`
	Data     any    `json:"data,omitempty"`
}

// AlertWorker subscribes to Redis output streams and fires webhooks
// based on the currently stored AlertConfig.
type AlertWorker struct {
	rdb   *redis.Client
	store *Store
}

func NewAlertWorker(rdb *redis.Client, store *Store) *AlertWorker {
	return &AlertWorker{rdb: rdb, store: store}
}

// Run starts the alert evaluation loop. Blocks until ctx is cancelled.
func (w *AlertWorker) Run(ctx context.Context) {
	lastIDs := map[string]string{
		"consensus:anomalies": "$",
		"consensus:status":    "$",
		"consensus:updates":   "$",
	}

	log.Println("alert worker started")

	for {
		if ctx.Err() != nil {
			return
		}

		cfg, err := w.store.GetAlertConfig(ctx)
		if err != nil || cfg.WebhookURL == "" {
			// No webhook configured — sleep and poll again.
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		streamArgs := []string{
			"consensus:anomalies", lastIDs["consensus:anomalies"],
			"consensus:status", lastIDs["consensus:status"],
			"consensus:updates", lastIDs["consensus:updates"],
		}

		results, err := w.rdb.XRead(ctx, &redis.XReadArgs{
			Streams: streamArgs,
			Count:   100,
			Block:   2 * time.Second,
		}).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("alert worker XRead: %v", err)
			time.Sleep(time.Second)
			continue
		}

		for _, stream := range results {
			lastIDs[stream.Stream] = stream.Messages[len(stream.Messages)-1].ID
			for _, msg := range stream.Messages {
				raw, ok := msg.Values["data"].(string)
				if !ok {
					continue
				}
				w.evaluate(ctx, cfg, stream.Stream, raw)
			}
		}
	}
}

func (w *AlertWorker) evaluate(ctx context.Context, cfg AlertConfig, stream, raw string) {
	var p WebhookPayload
	p.Source = "consensus-engine"
	p.TsMs = time.Now().UnixMilli()

	switch stream {
	case "consensus:anomalies":
		var a struct {
			Venue        string  `json:"venue"`
			Symbol       string  `json:"symbol"`
			AnomalyType  string  `json:"anomaly_type"`
			Severity     string  `json:"severity"`
			DeviationBps float64 `json:"deviation_bps"`
		}
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return
		}
		fire := (a.Severity == "HIGH" && cfg.OnAnomalyHigh) ||
			(a.Severity == "MEDIUM" && cfg.OnAnomalyMedium) ||
			(cfg.DeviationBpsThresh > 0 && a.DeviationBps >= cfg.DeviationBpsThresh)
		if !fire {
			return
		}
		p.Event = "anomaly_detected"
		p.Severity = a.Severity
		p.Message = fmt.Sprintf("[%s] %s anomaly on %s %s — %.1f bps deviation",
			a.Severity, a.AnomalyType, a.Venue, a.Symbol, a.DeviationBps)
		p.Data = a

	case "consensus:status":
		var su struct {
			Venue  string `json:"venue"`
			Symbol string `json:"symbol"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(raw), &su); err != nil {
			return
		}
		if !cfg.OnVenueBlacklisted || su.Status != "BLACKLISTED" {
			return
		}
		p.Event = "venue_blacklisted"
		p.Severity = "HIGH"
		p.Message = fmt.Sprintf("[BLACKLISTED] %s on %s — %s", su.Venue, su.Symbol, su.Reason)
		p.Data = su

	case "consensus:updates":
		if !cfg.OnQualityLow {
			return
		}
		var u struct {
			Symbol    string `json:"symbol"`
			Consensus struct {
				Quality string `json:"quality"`
			} `json:"consensus"`
		}
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			return
		}
		if u.Consensus.Quality != "LOW" {
			return
		}
		p.Event = "quality_low"
		p.Severity = "MEDIUM"
		p.Message = fmt.Sprintf("[LOW QUALITY] Consensus quality dropped to LOW for %s", u.Symbol)
		p.Data = u
	}

	if p.Event == "" {
		return
	}
	w.fire(ctx, cfg.WebhookURL, p)
}

func (w *AlertWorker) fire(ctx context.Context, url string, p WebhookPayload) {
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("webhook: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "consensus-engine-dashboard/1.0")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("webhook: delivery failed: %v", err)
		return
	}
	defer resp.Body.Close()
	log.Printf("webhook: %s → HTTP %d", p.Event, resp.StatusCode)
}

// TestWebhook sends a test payload to the given URL.
func (w *AlertWorker) TestWebhook(ctx context.Context, webhookURL string) error {
	p := WebhookPayload{
		Source:   "consensus-engine",
		Event:    "test",
		Severity: "INFO",
		Message:  "Test webhook from consensus-engine dashboard",
		TsMs:     time.Now().UnixMilli(),
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "consensus-engine-dashboard/1.0")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
