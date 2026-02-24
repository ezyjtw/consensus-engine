// Package pipeline provides infrastructure performance monitoring, latency
// tracking, and parallel execution primitives for the trading pipeline.
package pipeline

import (
	"sync"
	"sync/atomic"
	"time"
)

// Stage represents a named stage in the processing pipeline.
type Stage string

const (
	StageMarketData  Stage = "market_data"
	StageConsensus   Stage = "consensus"
	StageArb         Stage = "arb_detection"
	StageFunding     Stage = "funding_detection"
	StageAllocation  Stage = "allocation"
	StageExecution   Stage = "execution"
	StageFairValue   Stage = "fair_value"
	StagePrediction  Stage = "prediction"
	StageRouting     Stage = "routing"
)

// LatencyRecord records the duration of one pipeline stage invocation.
type LatencyRecord struct {
	Stage      Stage `json:"stage"`
	DurationUs int64 `json:"duration_us"`
	TsMs       int64 `json:"ts_ms"`
}

// LatencyReport summarises pipeline performance.
type LatencyReport struct {
	Stages         []StageLatency `json:"stages"`
	TotalP50Us     int64          `json:"total_p50_us"`
	TotalP99Us     int64          `json:"total_p99_us"`
	TickToTradeUs  int64          `json:"tick_to_trade_us"` // market_data → execution
	TsMs           int64          `json:"ts_ms"`
}

// StageLatency is latency statistics for one pipeline stage.
type StageLatency struct {
	Stage    Stage `json:"stage"`
	P50Us    int64 `json:"p50_us"`
	P95Us    int64 `json:"p95_us"`
	P99Us    int64 `json:"p99_us"`
	MaxUs    int64 `json:"max_us"`
	AvgUs    int64 `json:"avg_us"`
	Count    int   `json:"count"`
}

// LatencyTracker monitors pipeline performance in real time.
type LatencyTracker struct {
	mu       sync.RWMutex
	records  map[Stage][]LatencyRecord
	windowMs int64
	maxRecords int
}

// NewLatencyTracker creates a pipeline latency tracker.
func NewLatencyTracker(windowMs int64) *LatencyTracker {
	return &LatencyTracker{
		records:    make(map[Stage][]LatencyRecord),
		windowMs:   windowMs,
		maxRecords: 10000,
	}
}

// Record logs a latency observation.
func (lt *LatencyTracker) Record(stage Stage, durationUs int64) {
	lt.mu.Lock()
	defer lt.mu.Unlock()

	rec := LatencyRecord{
		Stage:      stage,
		DurationUs: durationUs,
		TsMs:       time.Now().UnixMilli(),
	}
	lt.records[stage] = append(lt.records[stage], rec)

	// Trim
	cutoff := time.Now().UnixMilli() - lt.windowMs
	recs := lt.records[stage]
	start := 0
	for start < len(recs) && recs[start].TsMs < cutoff {
		start++
	}
	if start > 0 {
		lt.records[stage] = recs[start:]
	}
	if len(lt.records[stage]) > lt.maxRecords {
		lt.records[stage] = lt.records[stage][len(lt.records[stage])-lt.maxRecords:]
	}
}

// Measure is a convenience that records the elapsed time since start.
func (lt *LatencyTracker) Measure(stage Stage, startNano int64) {
	elapsed := time.Now().UnixNano() - startNano
	lt.Record(stage, elapsed/1000) // convert to microseconds
}

// Report generates a latency report.
func (lt *LatencyTracker) Report() LatencyReport {
	lt.mu.RLock()
	defer lt.mu.RUnlock()

	now := time.Now().UnixMilli()
	cutoff := now - lt.windowMs

	stages := []Stage{StageMarketData, StageConsensus, StageArb, StageFunding,
		StageAllocation, StageExecution, StageFairValue, StagePrediction, StageRouting}

	var stageLatencies []StageLatency
	var totalP50, totalP99 int64

	for _, stage := range stages {
		recs := lt.records[stage]
		var durations []int64
		for _, r := range recs {
			if r.TsMs >= cutoff {
				durations = append(durations, r.DurationUs)
			}
		}

		if len(durations) == 0 {
			continue
		}

		sl := computeStageLatency(stage, durations)
		stageLatencies = append(stageLatencies, sl)
		totalP50 += sl.P50Us
		totalP99 += sl.P99Us
	}

	return LatencyReport{
		Stages:        stageLatencies,
		TotalP50Us:    totalP50,
		TotalP99Us:    totalP99,
		TickToTradeUs: totalP50, // approximation: sum of all stages
		TsMs:          now,
	}
}

func computeStageLatency(stage Stage, durations []int64) StageLatency {
	n := len(durations)
	if n == 0 {
		return StageLatency{Stage: stage}
	}

	// Sort for percentiles
	sorted := make([]int64, n)
	copy(sorted, durations)
	sortInt64(sorted)

	var sum int64
	for _, d := range sorted {
		sum += d
	}

	return StageLatency{
		Stage: stage,
		P50Us: sorted[n*50/100],
		P95Us: sorted[n*95/100],
		P99Us: sorted[min(n-1, n*99/100)],
		MaxUs: sorted[n-1],
		AvgUs: sum / int64(n),
		Count: n,
	}
}

func sortInt64(s []int64) {
	// Simple insertion sort for typical small arrays
	for i := 1; i < len(s); i++ {
		key := s[i]
		j := i - 1
		for j >= 0 && s[j] > key {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = key
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ParallelExecutor runs independent tasks concurrently with coordination.
type ParallelExecutor struct {
	maxConcurrency int
	sem            chan struct{}
}

// NewParallelExecutor creates a parallel executor with a concurrency limit.
func NewParallelExecutor(maxConcurrency int) *ParallelExecutor {
	return &ParallelExecutor{
		maxConcurrency: maxConcurrency,
		sem:            make(chan struct{}, maxConcurrency),
	}
}

// Task is a unit of work for parallel execution.
type Task struct {
	Name string
	Fn   func() error
}

// TaskResult captures the outcome of a parallel task.
type TaskResult struct {
	Name     string
	Err      error
	Duration time.Duration
}

// RunAll executes all tasks in parallel (up to max concurrency) and waits for all to complete.
func (pe *ParallelExecutor) RunAll(tasks []Task) []TaskResult {
	results := make([]TaskResult, len(tasks))
	var wg sync.WaitGroup

	for i, task := range tasks {
		wg.Add(1)
		go func(idx int, t Task) {
			defer wg.Done()
			pe.sem <- struct{}{}
			defer func() { <-pe.sem }()

			start := time.Now()
			err := t.Fn()
			results[idx] = TaskResult{
				Name:     t.Name,
				Err:      err,
				Duration: time.Since(start),
			}
		}(i, task)
	}

	wg.Wait()
	return results
}

// RingBuffer is a lock-free ring buffer for high-throughput data passing
// between producer and consumer goroutines.
type RingBuffer[T any] struct {
	data   []T
	size   int64
	head   atomic.Int64 // write position
	tail   atomic.Int64 // read position
}

// NewRingBuffer creates a ring buffer with the given capacity.
func NewRingBuffer[T any](capacity int) *RingBuffer[T] {
	return &RingBuffer[T]{
		data: make([]T, capacity),
		size: int64(capacity),
	}
}

// Push adds an item to the buffer. Returns false if buffer is full.
func (rb *RingBuffer[T]) Push(item T) bool {
	head := rb.head.Load()
	tail := rb.tail.Load()

	if head-tail >= rb.size {
		return false // full
	}

	rb.data[head%rb.size] = item
	rb.head.Add(1)
	return true
}

// Pop removes and returns the oldest item. Returns false if empty.
func (rb *RingBuffer[T]) Pop() (T, bool) {
	tail := rb.tail.Load()
	head := rb.head.Load()

	if tail >= head {
		var zero T
		return zero, false // empty
	}

	item := rb.data[tail%rb.size]
	rb.tail.Add(1)
	return item, true
}

// Len returns the number of items in the buffer.
func (rb *RingBuffer[T]) Len() int {
	return int(rb.head.Load() - rb.tail.Load())
}
