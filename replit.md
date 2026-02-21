# Consensus Engine

## Overview
Multi-venue crypto price consensus service built with Go. Provides a JSON API for service status and health checks.

## Project Architecture
- **Language**: Go 1.22+ (running on Go 1.25)
- **Framework**: Standard library `net/http`
- **Dependencies**: go-redis/redis v9, gopkg.in/yaml.v3 (declared in go.mod)
- **Entry point**: `main.go`

## Endpoints
- `GET /` - Service status and description
- `GET /health` - Health check

## Running
The application runs on port 5000 (configurable via PORT environment variable).

## Recent Changes
- 2026-02-21: Initial Replit setup with Go 1.25, created main.go entry point, configured workflow and deployment.
