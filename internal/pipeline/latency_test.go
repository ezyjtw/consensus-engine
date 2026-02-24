package pipeline

import (
	"errors"
	"testing"
	"time"
)

func TestLatencyTracker(t *testing.T) {
	lt := NewLatencyTracker(60000)

	// Record some latencies
	for i := 0; i < 100; i++ {
		lt.Record(StageConsensus, int64(100+i*10)) // 100us-1090us
		lt.Record(StageArb, int64(50+i*5))         // 50us-545us
	}

	report := lt.Report()
	if len(report.Stages) < 2 {
		t.Fatalf("expected at least 2 stages, got %d", len(report.Stages))
	}

	for _, s := range report.Stages {
		if s.Count != 100 {
			t.Errorf("%s: expected 100 samples, got %d", s.Stage, s.Count)
		}
		if s.P50Us == 0 {
			t.Errorf("%s: P50 should be non-zero", s.Stage)
		}
		if s.P99Us < s.P50Us {
			t.Errorf("%s: P99 (%d) < P50 (%d)", s.Stage, s.P99Us, s.P50Us)
		}
	}
}

func TestLatencyMeasure(t *testing.T) {
	lt := NewLatencyTracker(60000)

	start := time.Now().UnixNano()
	time.Sleep(time.Millisecond) // ~1000us
	lt.Measure(StageExecution, start)

	report := lt.Report()
	found := false
	for _, s := range report.Stages {
		if s.Stage == StageExecution {
			found = true
			if s.Count != 1 {
				t.Errorf("expected 1 record, got %d", s.Count)
			}
			if s.P50Us < 500 { // at least 500us (sleep might be imprecise)
				t.Errorf("expected >= 500us latency, got %d", s.P50Us)
			}
		}
	}
	if !found {
		t.Error("expected execution stage in report")
	}
}

func TestParallelExecutor(t *testing.T) {
	pe := NewParallelExecutor(4)

	tasks := []Task{
		{Name: "t1", Fn: func() error { time.Sleep(10 * time.Millisecond); return nil }},
		{Name: "t2", Fn: func() error { time.Sleep(10 * time.Millisecond); return nil }},
		{Name: "t3", Fn: func() error { time.Sleep(10 * time.Millisecond); return errors.New("oops") }},
		{Name: "t4", Fn: func() error { return nil }},
	}

	start := time.Now()
	results := pe.RunAll(tasks)
	elapsed := time.Since(start)

	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}

	// All ran in parallel → total time should be < 4x single task
	if elapsed > 80*time.Millisecond {
		t.Errorf("expected parallel execution, took %v", elapsed)
	}

	// Task 3 should have error
	if results[2].Err == nil {
		t.Error("expected error from task t3")
	}
}

func TestRingBuffer(t *testing.T) {
	rb := NewRingBuffer[int](4)

	// Fill
	for i := 0; i < 4; i++ {
		if !rb.Push(i) {
			t.Fatalf("push failed at %d", i)
		}
	}

	// Should be full
	if rb.Push(99) {
		t.Error("push should fail when full")
	}

	if rb.Len() != 4 {
		t.Errorf("expected len 4, got %d", rb.Len())
	}

	// Drain
	for i := 0; i < 4; i++ {
		val, ok := rb.Pop()
		if !ok {
			t.Fatalf("pop failed at %d", i)
		}
		if val != i {
			t.Errorf("expected %d, got %d", i, val)
		}
	}

	// Should be empty
	_, ok := rb.Pop()
	if ok {
		t.Error("pop should fail when empty")
	}
}
