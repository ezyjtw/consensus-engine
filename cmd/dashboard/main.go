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
	"github.com/yourorg/arbsuite/internal/dashboard"
)

func main() {
	masterKeyHex := mustEnv("DASHBOARD_MASTER_KEY")
	masterKey, err := dashboard.ParseKey(masterKeyHex)
	if err != nil {
		log.Fatalf("DASHBOARD_MASTER_KEY: %v", err)
	}

	authToken := os.Getenv("DASHBOARD_AUTH_TOKEN")
	if authToken == "" {
		log.Println("WARNING: DASHBOARD_AUTH_TOKEN not set — dashboard is unprotected")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go alerts.Run(ctx)

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
