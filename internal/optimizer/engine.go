// Package optimizer provides a self-optimising parameter engine that
// continuously tunes strategy parameters based on realised performance.
package optimizer

import (
	"math"
	"sync"
	"time"
)

// Parameter is a tunable strategy parameter.
type Parameter struct {
	Name     string  `json:"name"`
	Current  float64 `json:"current"`
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	StepSize float64 `json:"step_size"`
}

// PerformanceSample records outcome data for one parameter configuration.
type PerformanceSample struct {
	Params    map[string]float64 `json:"params"`
	PnLUSD   float64            `json:"pnl_usd"`
	Sharpe   float64            `json:"sharpe"`
	FillRate float64            `json:"fill_rate"` // 0..1
	TsMs     int64              `json:"ts_ms"`
}

// OptimisationResult is the output of one tuning cycle.
type OptimisationResult struct {
	Adjustments  map[string]float64 `json:"adjustments"`
	PrevSharpe   float64            `json:"prev_sharpe"`
	NewSharpe    float64            `json:"new_sharpe"`
	SamplesUsed  int                `json:"samples_used"`
	Confidence   float64            `json:"confidence"` // 0..1
	TsMs         int64              `json:"ts_ms"`
}

// Engine continuously optimises strategy parameters via online gradient estimation.
type Engine struct {
	mu         sync.RWMutex
	params     map[string]*Parameter
	samples    []PerformanceSample
	windowMs   int64
	maxSamples int
	minSamples int    // minimum samples before tuning
	stepDecay  float64 // reduce step size over time
}

// NewEngine creates an optimizer with tunable parameters.
func NewEngine(params []Parameter, windowMs int64) *Engine {
	pm := make(map[string]*Parameter, len(params))
	for i := range params {
		p := params[i]
		pm[p.Name] = &p
	}
	return &Engine{
		params:     pm,
		windowMs:   windowMs,
		maxSamples: 5000,
		minSamples: 20,
		stepDecay:  0.999,
	}
}

// Record adds a performance observation.
func (e *Engine) Record(s PerformanceSample) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.samples = append(e.samples, s)
	e.trimOld()
}

// Optimise runs one tuning cycle and returns parameter adjustments.
// Uses finite difference gradient estimation on the Sharpe proxy.
func (e *Engine) Optimise() *OptimisationResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	cutoff := now - e.windowMs

	var recent []PerformanceSample
	for _, s := range e.samples {
		if s.TsMs >= cutoff {
			recent = append(recent, s)
		}
	}

	if len(recent) < e.minSamples {
		return nil
	}

	// Split into two halves for comparison
	half := len(recent) / 2
	firstHalf := recent[:half]
	secondHalf := recent[half:]

	prevSharpe := avgSharpe(firstHalf)
	currSharpe := avgSharpe(secondHalf)

	// Estimate gradient for each parameter via correlation with PnL
	adjustments := make(map[string]float64)

	for name, param := range e.params {
		grad := e.estimateGradient(name, recent)

		// Step in gradient direction, bounded by min/max
		step := param.StepSize * grad
		newVal := param.Current + step
		newVal = math.Max(param.Min, math.Min(param.Max, newVal))

		if newVal != param.Current {
			adjustments[name] = newVal
			param.Current = newVal
			param.StepSize *= e.stepDecay // shrink step over time
		}
	}

	confidence := math.Min(1, float64(len(recent))/100)

	return &OptimisationResult{
		Adjustments: adjustments,
		PrevSharpe:  prevSharpe,
		NewSharpe:   currSharpe,
		SamplesUsed: len(recent),
		Confidence:  confidence,
		TsMs:        now,
	}
}

// CurrentParams returns all current parameter values.
func (e *Engine) CurrentParams() map[string]float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := make(map[string]float64, len(e.params))
	for name, p := range e.params {
		result[name] = p.Current
	}
	return result
}

// estimateGradient computes the approximate gradient of Sharpe with respect to
// a parameter using correlation between parameter value and outcome.
func (e *Engine) estimateGradient(paramName string, samples []PerformanceSample) float64 {
	if len(samples) < 5 {
		return 0
	}

	var xs, ys []float64
	for _, s := range samples {
		v, ok := s.Params[paramName]
		if !ok {
			continue
		}
		xs = append(xs, v)
		ys = append(ys, s.PnLUSD)
	}

	if len(xs) < 5 {
		return 0
	}

	// Compute correlation (sign indicates gradient direction)
	n := float64(len(xs))
	var sumX, sumY, sumXY, sumXX, sumYY float64
	for i := range xs {
		sumX += xs[i]
		sumY += ys[i]
		sumXY += xs[i] * ys[i]
		sumXX += xs[i] * xs[i]
		sumYY += ys[i] * ys[i]
	}

	num := n*sumXY - sumX*sumY
	den := math.Sqrt((n*sumXX - sumX*sumX) * (n*sumYY - sumY*sumY))
	if den == 0 {
		return 0
	}

	corr := num / den
	return math.Max(-1, math.Min(1, corr))
}

func avgSharpe(samples []PerformanceSample) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += s.Sharpe
	}
	return sum / float64(len(samples))
}

func (e *Engine) trimOld() {
	cutoff := time.Now().UnixMilli() - e.windowMs
	start := 0
	for start < len(e.samples) && e.samples[start].TsMs < cutoff {
		start++
	}
	if start > 0 {
		e.samples = e.samples[start:]
	}
	if len(e.samples) > e.maxSamples {
		e.samples = e.samples[len(e.samples)-e.maxSamples:]
	}
}
