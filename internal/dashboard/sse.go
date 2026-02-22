package dashboard

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

// sseEvent is the envelope pushed to the browser via Server-Sent Events.
type sseEvent struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// StreamHandler serves an SSE stream that fans out Redis stream messages to
// the browser in real time.
type StreamHandler struct {
	rdb   *redis.Client
	store *Store
}

func NewStreamHandler(rdb *redis.Client, store *Store) *StreamHandler {
	return &StreamHandler{rdb: rdb, store: store}
}

var redisStreams = []string{
	"consensus:updates",
	"consensus:anomalies",
	"consensus:status",
}

func (h *StreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	// Send initial kill switch state so the UI is correct on first load.
	if ks, err := h.store.GetKillSwitch(ctx); err == nil {
		h.write(w, flusher, "kill_switch", ks)
	}

	// Start reading from the current tail of each stream ("$" = only new msgs).
	lastIDs := map[string]string{}
	for _, s := range redisStreams {
		lastIDs[s] = "$"
	}

	heartbeat := time.NewTicker(25 * time.Second)
	killPoll := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()
	defer killPoll.Stop()

	for {
		// Non-blocking check of timers before the blocking Redis read.
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-killPoll.C:
			if ks, err := h.store.GetKillSwitch(ctx); err == nil {
				h.write(w, flusher, "kill_switch", ks)
			}
		default:
		}

		// Build the alternating streams+ids slice required by XRead.
		streamArgs := make([]string, 0, len(redisStreams)*2)
		for _, s := range redisStreams {
			streamArgs = append(streamArgs, s)
		}
		for _, s := range redisStreams {
			streamArgs = append(streamArgs, lastIDs[s])
		}

		results, err := h.rdb.XRead(ctx, &redis.XReadArgs{
			Streams: streamArgs,
			Count:   50,
			Block:   500 * time.Millisecond,
		}).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("sse: XRead error: %v", err)
			time.Sleep(time.Second)
			continue
		}

		for _, stream := range results {
			lastIDs[stream.Stream] = stream.Messages[len(stream.Messages)-1].ID
			evType := streamEventType(stream.Stream)
			for _, msg := range stream.Messages {
				raw, ok := msg.Values["data"].(string)
				if !ok {
					continue
				}
				var payload any
				if err := json.Unmarshal([]byte(raw), &payload); err != nil {
					continue
				}
				h.write(w, flusher, evType, payload)
			}
		}
	}
}

func (h *StreamHandler) write(w http.ResponseWriter, f http.Flusher, evType string, data any) {
	ev := sseEvent{Type: evType, Data: data}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
	f.Flush()
}

func streamEventType(stream string) string {
	switch stream {
	case "consensus:updates":
		return "consensus"
	case "consensus:anomalies":
		return "anomaly"
	case "consensus:status":
		return "status"
	default:
		return "unknown"
	}
}
