# Consensus Engine

## Overview
Multi-venue crypto price consensus service built with Go. Aggregates price quotes from multiple cryptocurrency exchanges, computes trust-weighted consensus prices, and detects anomalies via circuit breaker logic.

## Project Architecture
- **Language**: Go 1.22+ (running on Go 1.25)
- **Framework**: Standard library `net/http` for HTTP API
- **Dependencies**: redis/go-redis v9 (Redis streams), gopkg.in/yaml.v3 (config)
- **Entry point (HTTP)**: `main.go` - serves HTTP API on port 5000
- **Entry point (Engine)**: `cmd/consensus-engine/main.go` - Redis stream consumer

### Directory Structure
```
main.go                          # HTTP API server (port 5000)
cmd/consensus-engine/main.go     # Redis stream consumer (production engine)
configs/policies/                # YAML policy configuration
internal/consensus/              # Core consensus logic
  types.go                       # Data types and structs
  config.go                      # Policy loading from YAML
  math.go                        # Statistical functions (median, MAD, etc.)
  price.go                       # VWAP and executable price computation
  trust.go                       # Trust scoring and normalization
  circuitbreaker.go              # Venue status state machine
  engine.go                      # Main consensus computation
internal/store/store.go          # In-memory quote and status storage
internal/eventbus/redis.go       # Redis streams pub/sub
```

## HTTP Endpoints
- `GET /` - Service info, policy status, configured symbols and venues
- `GET /health` - Health check
- `GET /config` - Current policy configuration

## Configuration
Policy is loaded from `configs/policies/consensus_policy.yaml`. Redis address and password can be overridden with `REDIS_ADDR` and `REDIS_PASSWORD` environment variables.

## Running
The HTTP server runs on port 5000 (configurable via `PORT` environment variable).

## Recent Changes
- 2026-02-21: Added all core source files (consensus engine, store, eventbus), fixed redis import path (go-redis/redis -> redis/go-redis), created policy YAML config, updated HTTP server to expose policy info.
