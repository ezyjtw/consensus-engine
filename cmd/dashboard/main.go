package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/redis/go-redis/v9"
	"github.com/ezyjtw/consensus-engine/internal/dashboard"
	"github.com/ezyjtw/consensus-engine/internal/ledger"
)

func main() {
	masterKeyHex := mustEnv("DASHBOARD_MASTER_KEY")
	masterKey, err := dashboard.ParseKey(masterKeyHex)
	if err != nil {
		log.Fatalf("DASHBOARD_MASTER_KEY: %v", err)
	}

	authToken := os.Getenv("DASHBOARD_AUTH_TOKEN")
	env := os.Getenv("ENV")
	if authToken == "" {
		switch env {
		case "", "dev", "development":
			log.Println("WARNING: DASHBOARD_AUTH_TOKEN not set — running in dev mode (unprotected)")
		default:
			log.Fatalf("DASHBOARD_AUTH_TOKEN is required when ENV=%s (set ENV=dev for development mode)", env)
		}
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	tenantID := os.Getenv("TENANT_ID")
	if tenantID == "" {
		tenantID = "default"
	}

	rdbOpts, err := buildRedisOpts()
	if err != nil {
		log.Fatalf("redis config: %v", err)
	}
	rdb := redis.NewClient(rdbOpts)

	// Verify Redis connectivity at startup.
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis ping failed: %v", err)
	}
	log.Printf("redis connected: %s", rdbOpts.Addr)

	store := dashboard.NewStore(rdb, masterKey)
	sse := dashboard.NewStreamHandler(rdb, store)
	alerts := dashboard.NewAlertWorker(rdb, store)
	srv := dashboard.NewServer(store, sse, alerts, authToken)

	// Optionally connect to Postgres for the Gateway API historical endpoints.
	var ldb *ledger.DB
	if pgDSN := os.Getenv("POSTGRES_DSN"); pgDSN != "" {
		ctx := context.Background()
		ldb, err = ledger.Connect(ctx, pgDSN)
		if err != nil {
			log.Printf("WARNING: postgres connect failed (%v) — history endpoints will return empty", err)
			ldb = nil
		} else {
			log.Println("postgres connected — Gateway API historical endpoints enabled")
		}
	} else {
		log.Println("POSTGRES_DSN not set — Gateway API will use Redis-only data")
	}

	// Wire the Gateway API routes onto the dashboard server.
	gw := dashboard.NewGateway(rdb, ldb, tenantID)
	srv.RegisterGateway(gw)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go alerts.Run(ctx)

	// Background price poller — fetches live prices from public exchange REST
	// APIs so the dashboard shows real-time data even without the dedicated
	// market-data WebSocket service running.
	poller := dashboard.NewPricePoller(rdb, tenantID, 0)
	go poller.Run(ctx)

	httpSrv := &http.Server{
		Addr:    ":" + port,
		Handler: srv,
	}

	go func() {
		log.Printf("dashboard listening on :%s", port)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("dashboard shutting down…")
	httpSrv.Shutdown(context.Background()) //nolint:errcheck

	if ldb != nil {
		ldb.Close()
	}
}

func buildRedisOpts() (*redis.Options, error) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	if strings.HasPrefix(addr, "redis://") || strings.HasPrefix(addr, "rediss://") {
		opts, err := redis.ParseURL(addr)
		if err != nil {
			return nil, fmt.Errorf("parsing REDIS_ADDR: %w", err)
		}
		if pw := os.Getenv("REDIS_PASSWORD"); pw != "" {
			opts.Password = pw
		}
		if os.Getenv("REDIS_TLS") == "true" && opts.TLSConfig == nil {
			opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		return opts, nil
	}
	opts := &redis.Options{
		Addr:     addr,
		Password: os.Getenv("REDIS_PASSWORD"),
	}
	if os.Getenv("REDIS_TLS") == "true" {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return opts, nil
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}
