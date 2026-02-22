package main

import (
        "context"
        "flag"
        "log"
        "os"
        "os/signal"
        "strings"
        "syscall"
        "time"

        "github.com/yourorg/consensus-engine/internal/consensus"
        "github.com/yourorg/consensus-engine/internal/eventbus"
        "github.com/yourorg/consensus-engine/internal/store"
)

func main() {
        cfgPath := flag.String("config",
                "configs/policies/consensus_policy.yaml", "Path to policy YAML")
        flag.Parse()

        if addr := os.Getenv("REDIS_ADDR"); addr != "" {
                log.Printf("REDIS_ADDR detected: %s", addr)
        } else {
                log.Println("REDIS_ADDR not set, using default from policy file")
        }
        if os.Getenv("REDIS_PASSWORD") != "" {
                log.Println("REDIS_PASSWORD detected")
        } else {
                log.Println("REDIS_PASSWORD not set, using default from policy file")
        }
        if tls := os.Getenv("REDIS_TLS"); tls != "" {
                log.Printf("REDIS_TLS detected: %q (must be exactly \"true\" to enable TLS)", tls)
        } else {
                log.Println("REDIS_TLS not set, TLS disabled unless policy file sets use_tls: true")
        }

        policy, err := consensus.LoadPolicy(*cfgPath)
        if err != nil {
                log.Fatalf("failed to load policy: %v", err)
        }

        log.Printf("Active policy: stale_ms=%d outlier_warn=%.0f bps outlier_blacklist=%.0f bps "+
                "warn_persist=%dms blacklist_persist=%dms blacklist_ttl=%dms recovery=%dms "+
                "min_core_quorum=%d core_venues=[%s] tls=%v",
                policy.StaleMs,
                policy.OutlierBpsWarn, policy.OutlierBpsBlacklist,
                policy.WarnPersistMs, policy.BlacklistPersistMs,
                policy.BlacklistTtlMs, policy.RecoveryMs,
                policy.MinCoreQuorum,
                strings.Join(policy.CoreVenues, ","),
                policy.Redis.UseTLS,
        )
        log.Printf("Redis address: %s", policy.Redis.Addr)

        quoteStore := store.New(policy.StaleMs)
        engine := consensus.NewEngine(policy)

        bus, err := eventbus.New(eventbus.RedisConfig{
                Addr:            policy.Redis.Addr,
                Password:        policy.Redis.Password,
                UseTLS:          policy.Redis.UseTLS,
                InputStream:     policy.Redis.InputStream,
                OutputConsensus: policy.Redis.OutputConsensus,
                OutputAnomalies: policy.Redis.OutputAnomalies,
                OutputStatus:    policy.Redis.OutputStatus,
                ConsumerGroup:   policy.Redis.ConsumerGroup,
                ConsumerName:    policy.Redis.ConsumerName,
                BlockMs:         time.Duration(policy.Redis.BlockMs) * time.Millisecond,
                BatchSize:       policy.Redis.BatchSize,
        })
        if err != nil {
                log.Fatalf("failed to create event bus: %v", err)
        }
        defer func() {
                if err := bus.Close(); err != nil {
                        log.Printf("error closing event bus: %v", err)
                }
        }()

        ctx, cancel := signal.NotifyContext(context.Background(),
                os.Interrupt, syscall.SIGTERM)
        defer cancel()

        log.Println("consensus-engine started...")

        type symKey struct {
                tenantID string
                symbol   consensus.Symbol
        }

        for {
                select {
                case <-ctx.Done():
                        log.Println("shutting down")
                        return
                default:
                }

                quotes, err := bus.ReadMarketUpdates(ctx)
                if err != nil {
                        log.Printf("read error: %v", err)
                        time.Sleep(100 * time.Millisecond)
                        continue
                }
                if len(quotes) == 0 {
                        continue
                }

                toRecompute := make(map[symKey]bool)
                for _, q := range quotes {
                        quoteStore.UpsertQuote(q)
                        toRecompute[symKey{tenantID: q.TenantID,
                                symbol: q.Symbol}] = true
                }

                for sk := range toRecompute {
                        liveQuotes := quoteStore.LiveQuotes(sk.tenantID, sk.symbol)
                        if len(liveQuotes) == 0 {
                                continue
                        }
                        result := engine.Compute(sk.tenantID, sk.symbol, liveQuotes,
                                func(v consensus.Venue) consensus.VenueStatus {
                                        return quoteStore.GetStatus(sk.tenantID, sk.symbol, v)
                                })

                        for v, ns := range result.NewStatuses {
                                quoteStore.SetStatus(sk.tenantID, sk.symbol, v, ns)
                        }
                        if result.Update.Consensus.Mid == 0 {
                                log.Printf("consensus unavailable for %s/%s: all venues have zero trust",
                                        sk.tenantID, sk.symbol)
                        } else if err := bus.PublishConsensus(ctx, result.Update); err != nil {
                                log.Printf("publish consensus error: %v", err)
                        }
                        for _, a := range result.Anomalies {
                                if err := bus.PublishAnomaly(ctx, a); err != nil {
                                        log.Printf("publish anomaly error: %v", err)
                                }
                        }
                        for _, su := range result.StatusUpdates {
                                if err := bus.PublishStatusUpdate(ctx, su); err != nil {
                                        log.Printf("publish status error: %v", err)
                                }
                        }
                }
        }
}
