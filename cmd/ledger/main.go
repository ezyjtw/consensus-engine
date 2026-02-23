package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourorg/arbsuite/internal/eventbus"
	"github.com/yourorg/arbsuite/internal/execution"
	"github.com/yourorg/arbsuite/internal/ledger"
	"github.com/yourorg/arbsuite/internal/risk"
)

func main() {
	pgDSN := flag.String("dsn", "", "Postgres DSN (overrides POSTGRES_DSN env)")
	flag.Parse()

	if v := os.Getenv("REDIS_ADDR"); v != "" {
		log.Printf("REDIS_ADDR: %s", v)
	}
	if *pgDSN == "" {
		*pgDSN = os.Getenv("POSTGRES_DSN")
	}
	if *pgDSN == "" {
		*pgDSN = "postgres://arbsuite:arbsuite@localhost:5432/arbsuite?sslmode=disable"
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	db, err := ledger.Connect(ctx, *pgDSN)
	if err != nil {
		log.Fatalf("ledger: postgres connect: %v", err)
	}
	defer db.Close()
	log.Println("ledger: postgres connected and schema applied")

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	redisPw := os.Getenv("REDIS_PASSWORD")
	redisTLS := os.Getenv("REDIS_TLS") == "true"
	tenantID := os.Getenv("TENANT_ID")
	if tenantID == "" {
		tenantID = "default"
	}

	// One StreamClient subscribing to all relevant streams.
	sc, err := eventbus.NewStreamClient(redisAddr, redisPw, redisTLS)
	if err != nil {
		log.Fatalf("ledger: redis connect: %v", err)
	}
	defer sc.Close()

	group := "ledger"
	streams := []string{
		"execution:events",
		"demo:fills",
		"live:fills",
		"risk:alerts",
		"risk:state",
		"trade:intents:approved",
	}
	for _, s := range streams {
		if err := sc.EnsureConsumerGroup(ctx, s, group); err != nil {
			log.Printf("ledger: ensure group on %q: %v (ok if stream not yet created)", s, err)
		}
	}

	log.Println("ledger: started — persisting events to Postgres")

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("ledger: shutdown")
			return
		case <-ticker.C:
			drainAll(ctx, sc, db, group, "worker-1", streams, tenantID)
		}
	}
}

func drainAll(ctx context.Context, sc *eventbus.StreamClient, db *ledger.DB,
	group, consumer string, streams []string, tenantID string) {

	for _, stream := range streams {
		msgs, err := sc.ReadMessages(ctx, stream, group, consumer, 50, 10*time.Millisecond)
		if err != nil || len(msgs) == 0 {
			continue
		}
		for _, m := range msgs {
			raw, ok := m.Values["data"].(string)
			if !ok {
				sc.Ack(ctx, stream, group, m.ID) //nolint:errcheck
				continue
			}
			persistMsg(ctx, db, stream, raw, tenantID)
			sc.Ack(ctx, stream, group, m.ID) //nolint:errcheck
		}
	}
}

func persistMsg(ctx context.Context, db *ledger.DB, stream, raw, tenantID string) {
	switch stream {
	case "execution:events":
		var ev execution.ExecutionEvent
		if err := json.Unmarshal([]byte(raw), &ev); err == nil {
			if err := db.WriteExecutionEvent(ctx, ev); err != nil {
				log.Printf("ledger: write exec event: %v", err)
			}
		}
	case "demo:fills", "live:fills":
		var fill execution.SimulatedFill
		if err := json.Unmarshal([]byte(raw), &fill); err == nil {
			if err := db.WriteFill(ctx, &fill); err != nil {
				log.Printf("ledger: write fill: %v", err)
			}
		}
	case "risk:alerts":
		var alert risk.Alert
		if err := json.Unmarshal([]byte(raw), &alert); err == nil {
			if err := db.WriteAlert(ctx, alert); err != nil {
				log.Printf("ledger: write alert: %v", err)
			}
		}
	case "risk:state":
		var state risk.State
		if err := json.Unmarshal([]byte(raw), &state); err == nil {
			if err := db.WriteRiskState(ctx, state); err != nil {
				log.Printf("ledger: write risk state: %v", err)
			}
		}
	}
}
