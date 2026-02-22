package main

import (
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/redis/go-redis/v9"
	"github.com/yourorg/consensus-engine/internal/dashboard"
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

	rdbOpts := &redis.Options{
		Addr:     redisAddr(),
		Password: os.Getenv("REDIS_PASSWORD"),
	}
	if os.Getenv("REDIS_TLS") == "true" {
		rdbOpts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
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

func redisAddr() string {
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		return v
	}
	return "localhost:6379"
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}
