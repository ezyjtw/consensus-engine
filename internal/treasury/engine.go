package treasury

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/exchange"
	"github.com/ezyjtw/consensus-engine/internal/eventbus"
)

// Engine runs the treasury pipeline: detect deposits → convert → distribute.
type Engine struct {
	cfg      *Config
	registry *exchange.Registry
	bus      *eventbus.StreamClient
	seen     *LastSeen
}

// NewEngine creates a treasury engine.
func NewEngine(cfg *Config, registry *exchange.Registry, bus *eventbus.StreamClient) *Engine {
	return &Engine{
		cfg:      cfg,
		registry: registry,
		bus:      bus,
		seen:     NewLastSeen(),
	}
}

// RunDepositWatcher polls the treasury venue for new deposits and triggers
// the convert→distribute pipeline for each.
func (e *Engine) RunDepositWatcher(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(e.cfg.PollIntervalSec) * time.Second)
	defer ticker.Stop()

	log.Printf("treasury: deposit watcher started (poll=%ds, venue=%s)",
		e.cfg.PollIntervalSec, e.cfg.TreasuryVenue)

	for {
		select {
		case <-ctx.Done():
			log.Printf("treasury: deposit watcher stopped")
			return
		case <-ticker.C:
			e.seen.Prune(24 * time.Hour)
			if err := e.pollDeposits(ctx); err != nil {
				log.Printf("treasury: poll error: %v", err)
			}
		}
	}
}

func (e *Engine) pollDeposits(ctx context.Context) error {
	ex, err := e.registry.Get(ctx, e.cfg.TreasuryVenue)
	if err != nil {
		return fmt.Errorf("getting treasury venue: %w", err)
	}

	// Check for fiat deposits if supported.
	if watcher, ok := ex.(exchange.DepositWatcher); ok {
		for _, ccy := range []string{"GBP", "USD", "EUR"} {
			deposits, err := watcher.GetFiatDeposits(ctx, ccy, 10)
			if err != nil {
				log.Printf("treasury: fiat deposit check (%s) failed: %v", ccy, err)
				continue
			}
			for _, d := range deposits {
				if d.Status != exchange.TransferCompleted || e.seen.Seen(d.DepositID) {
					continue
				}
				e.seen.Mark(d.DepositID)
				log.Printf("treasury: detected fiat deposit %s: %.2f %s",
					d.DepositID, d.Amount, d.Asset)
				e.processDeposit(ctx, d)
			}
		}
	}

	// Also check for crypto deposits (USDC, USDT).
	for _, asset := range []string{"USDC", "USDT"} {
		deposits, err := ex.GetDeposits(ctx, asset, 10)
		if err != nil {
			log.Printf("treasury: crypto deposit check (%s) failed: %v", asset, err)
			continue
		}
		for _, d := range deposits {
			if d.Status != exchange.TransferCompleted || e.seen.Seen(d.DepositID) {
				continue
			}
			e.seen.Mark(d.DepositID)
			log.Printf("treasury: detected crypto deposit %s: %.2f %s",
				d.DepositID, d.Amount, d.Asset)
			e.processDeposit(ctx, d)
		}
	}

	return nil
}

func (e *Engine) processDeposit(ctx context.Context, deposit exchange.DepositRecord) {
	now := time.Now().UnixMilli()

	// Publish deposit event.
	evt := DepositEvent{
		TenantID:  e.cfg.TenantID,
		DepositID: deposit.DepositID,
		Source:    e.cfg.TreasuryVenue,
		Asset:     deposit.Asset,
		Amount:    deposit.Amount,
		TxID:      deposit.TxID,
		Status:    "DETECTED",
		TsMs:      now,
	}
	if err := e.bus.Publish(ctx, "treasury:deposits", evt); err != nil {
		log.Printf("treasury: publish deposit event: %v", err)
	}

	// Convert fiat to target stablecoin if needed.
	amountUSD := deposit.Amount
	targetAsset := deposit.Asset

	if e.cfg.AutoConvert && deposit.Asset != e.cfg.ConvertTo {
		converted, err := e.convert(ctx, deposit.Asset, e.cfg.ConvertTo, deposit.Amount)
		if err != nil {
			log.Printf("treasury: conversion failed: %v", err)
			return
		}
		amountUSD = converted.ToAmount
		targetAsset = converted.ToAsset

		convEvt := ConversionEvent{
			TenantID:   e.cfg.TenantID,
			ConvertID:  converted.ConvertID,
			FromAsset:  converted.FromAsset,
			ToAsset:    converted.ToAsset,
			FromAmount: converted.FromAmount,
			ToAmount:   converted.ToAmount,
			FeesUSD:    converted.FeesUSD,
			TsMs:       time.Now().UnixMilli(),
		}
		if err := e.bus.Publish(ctx, "treasury:conversions", convEvt); err != nil {
			log.Printf("treasury: publish conversion event: %v", err)
		}

		log.Printf("treasury: converted %.2f %s → %.2f %s",
			deposit.Amount, deposit.Asset, amountUSD, targetAsset)
	}

	// Distribute to trading venues.
	if amountUSD < e.cfg.MinDistributeUSD {
		log.Printf("treasury: %.2f %s below min distribute threshold ($%.0f), holding",
			amountUSD, targetAsset, e.cfg.MinDistributeUSD)
		return
	}

	e.distribute(ctx, deposit.DepositID, targetAsset, amountUSD)
}

func (e *Engine) convert(ctx context.Context, from, to string, amount float64) (*exchange.ConvertResponse, error) {
	ex, err := e.registry.Get(ctx, e.cfg.TreasuryVenue)
	if err != nil {
		return nil, err
	}
	converter, ok := ex.(exchange.Converter)
	if !ok {
		return nil, fmt.Errorf("venue %s does not support conversion", e.cfg.TreasuryVenue)
	}
	return converter.Convert(ctx, exchange.ConvertRequest{
		FromAsset: from,
		ToAsset:   to,
		Amount:    amount,
	})
}

func (e *Engine) distribute(ctx context.Context, depositID, asset string, totalAmount float64) {
	now := time.Now().UnixMilli()
	var legs []DistributionLeg

	for _, alloc := range e.cfg.Allocation {
		amount := totalAmount * alloc.Weight
		if amount < 1.0 { // skip dust
			continue
		}

		ex, err := e.registry.Get(ctx, e.cfg.TreasuryVenue)
		if err != nil {
			log.Printf("treasury: distribute to %s: no client: %v", alloc.Venue, err)
			legs = append(legs, DistributionLeg{
				Venue:  alloc.Venue,
				Asset:  asset,
				Amount: amount,
				Status: "FAILED",
			})
			continue
		}

		// Get deposit address for target venue.
		targetEx, err := e.registry.Get(ctx, alloc.Venue)
		if err != nil {
			log.Printf("treasury: no client for target venue %s: %v", alloc.Venue, err)
			legs = append(legs, DistributionLeg{
				Venue:  alloc.Venue,
				Asset:  asset,
				Amount: amount,
				Status: "FAILED",
			})
			continue
		}

		depAddr, err := targetEx.GetDepositAddress(ctx, asset, alloc.Network)
		if err != nil {
			log.Printf("treasury: get deposit address for %s on %s: %v",
				alloc.Venue, alloc.Network, err)
			legs = append(legs, DistributionLeg{
				Venue:   alloc.Venue,
				Asset:   asset,
				Amount:  amount,
				Network: alloc.Network,
				Status:  "FAILED",
			})
			continue
		}

		// Withdraw from treasury venue to target venue.
		resp, err := ex.Withdraw(ctx, exchange.WithdrawRequest{
			Asset:   asset,
			Amount:  amount,
			Address: depAddr.Address,
			Network: alloc.Network,
			Tag:     depAddr.Tag,
		})
		if err != nil {
			log.Printf("treasury: withdraw to %s failed: %v", alloc.Venue, err)
			legs = append(legs, DistributionLeg{
				Venue:   alloc.Venue,
				Asset:   asset,
				Amount:  amount,
				Network: alloc.Network,
				Status:  "FAILED",
			})
			continue
		}

		log.Printf("treasury: sent %.2f %s → %s via %s (withdraw=%s)",
			amount, asset, alloc.Venue, alloc.Network, resp.WithdrawID)

		legs = append(legs, DistributionLeg{
			Venue:      alloc.Venue,
			Asset:      asset,
			Amount:     amount,
			Network:    alloc.Network,
			WithdrawID: resp.WithdrawID,
			Status:     "SENT",
		})
	}

	distEvt := DistributionEvent{
		TenantID:    e.cfg.TenantID,
		DepositID:   depositID,
		TotalAmount: totalAmount,
		Legs:        legs,
		TsMs:        now,
	}
	if err := e.bus.Publish(ctx, "treasury:distributions", distEvt); err != nil {
		log.Printf("treasury: publish distribution: %v", err)
	}
}

// RunSweep periodically checks trading venues for excess profit and sweeps
// it back to the treasury venue.
func (e *Engine) RunSweep(ctx context.Context) {
	if !e.cfg.SweepEnabled {
		log.Printf("treasury: sweep disabled")
		return
	}

	ticker := time.NewTicker(time.Duration(e.cfg.SweepIntervalMin) * time.Minute)
	defer ticker.Stop()

	log.Printf("treasury: sweep started (interval=%dm, threshold=$%.0f)",
		e.cfg.SweepIntervalMin, e.cfg.SweepThresholdUSD)

	for {
		select {
		case <-ctx.Done():
			log.Printf("treasury: sweep stopped")
			return
		case <-ticker.C:
			e.executeSweep(ctx)
		}
	}
}

func (e *Engine) executeSweep(ctx context.Context) {
	treasuryEx, err := e.registry.Get(ctx, e.cfg.TreasuryVenue)
	if err != nil {
		log.Printf("treasury: sweep: no treasury venue: %v", err)
		return
	}

	// Get deposit address on treasury venue.
	depAddr, err := treasuryEx.GetDepositAddress(ctx, e.cfg.ConvertTo, "arbitrum")
	if err != nil {
		log.Printf("treasury: sweep: get deposit address: %v", err)
		return
	}

	for _, alloc := range e.cfg.Allocation {
		ex, err := e.registry.Get(ctx, alloc.Venue)
		if err != nil {
			continue
		}

		balances, err := ex.GetBalances(ctx)
		if err != nil {
			log.Printf("treasury: sweep: get balances from %s: %v", alloc.Venue, err)
			continue
		}

		for _, bal := range balances {
			if bal.Asset != e.cfg.ConvertTo {
				continue
			}
			// Sweep free balance above threshold, keeping a reserve.
			sweepable := bal.Free - e.cfg.SweepThresholdUSD
			if sweepable <= 0 {
				continue
			}

			resp, err := ex.Withdraw(ctx, exchange.WithdrawRequest{
				Asset:   e.cfg.ConvertTo,
				Amount:  sweepable,
				Address: depAddr.Address,
				Network: alloc.Network,
				Tag:     depAddr.Tag,
			})
			if err != nil {
				log.Printf("treasury: sweep from %s failed: %v", alloc.Venue, err)
				continue
			}

			log.Printf("treasury: swept %.2f %s from %s → %s (withdraw=%s)",
				sweepable, e.cfg.ConvertTo, alloc.Venue, e.cfg.TreasuryVenue, resp.WithdrawID)

			sweepEvt := SweepEvent{
				TenantID:   e.cfg.TenantID,
				FromVenue:  alloc.Venue,
				ToVenue:    e.cfg.TreasuryVenue,
				Asset:      e.cfg.ConvertTo,
				Amount:     sweepable,
				WithdrawID: resp.WithdrawID,
				Status:     "SENT",
				TsMs:       time.Now().UnixMilli(),
			}
			if err := e.bus.Publish(ctx, "treasury:sweeps", sweepEvt); err != nil {
				log.Printf("treasury: publish sweep: %v", err)
			}
		}
	}
}

// RunReconciliation periodically checks balances across all venues and
// reports drift from expected values.
func (e *Engine) RunReconciliation(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(e.cfg.ReconcileIntervalMin) * time.Minute)
	defer ticker.Stop()

	log.Printf("treasury: reconciliation started (interval=%dm, drift_alert=%.1f%%)",
		e.cfg.ReconcileIntervalMin, e.cfg.DriftAlertPct)

	for {
		select {
		case <-ctx.Done():
			log.Printf("treasury: reconciliation stopped")
			return
		case <-ticker.C:
			report := e.reconcile(ctx)
			if err := e.bus.Publish(ctx, "treasury:reconciliation", report); err != nil {
				log.Printf("treasury: publish reconciliation: %v", err)
			}
			if !report.Healthy {
				for _, alert := range report.Alerts {
					log.Printf("treasury: ALERT: %s", alert)
				}
			}
		}
	}
}

func (e *Engine) reconcile(ctx context.Context) ReconciliationReport {
	now := time.Now().UnixMilli()
	report := ReconciliationReport{
		TenantID: e.cfg.TenantID,
		TsMs:     now,
		Healthy:  true,
	}

	allVenues := []string{e.cfg.TreasuryVenue}
	for _, a := range e.cfg.Allocation {
		allVenues = append(allVenues, a.Venue)
	}

	for _, venue := range allVenues {
		ex, err := e.registry.Get(ctx, venue)
		if err != nil {
			report.Alerts = append(report.Alerts,
				fmt.Sprintf("cannot connect to %s: %v", venue, err))
			report.Healthy = false
			continue
		}

		var balUSD float64
		balances, err := ex.GetBalances(ctx)
		if err != nil {
			report.Alerts = append(report.Alerts,
				fmt.Sprintf("cannot get balances from %s: %v", venue, err))
			report.Healthy = false
			continue
		}
		for _, b := range balances {
			balUSD += b.USDValue
			if b.USDValue == 0 {
				// Approximate stablecoin value.
				if b.Asset == "USDC" || b.Asset == "USDT" || b.Asset == "USD" {
					balUSD += b.Total
				}
			}
		}

		var posUSD float64
		positions, err := ex.GetPositions(ctx)
		if err == nil {
			for _, p := range positions {
				posUSD += math.Abs(p.NotionalUSD)
			}
		}

		vr := VenueReconcile{
			Venue:       venue,
			BalanceUSD:  balUSD,
			PositionUSD: posUSD,
		}

		// Check for excessive drift from expected allocation.
		// Expected is read from Redis key if available.
		expectedStr := e.bus.GetString(ctx,
			fmt.Sprintf("treasury:expected:%s:%s", e.cfg.TenantID, venue))
		if expectedStr != "" {
			var expected float64
			if _, err := fmt.Sscanf(expectedStr, "%f", &expected); err != nil {
				log.Printf("treasury: invalid expected value for %s: %v", venue, err)
				continue
			}
			vr.ExpectedUSD = expected
			vr.DriftUSD = balUSD - expected
			if expected > 0 {
				vr.DriftPct = (vr.DriftUSD / expected) * 100
			}
			if math.Abs(vr.DriftPct) > e.cfg.DriftAlertPct {
				report.Alerts = append(report.Alerts,
					fmt.Sprintf("%s drift %.1f%% ($%.2f)", venue, vr.DriftPct, vr.DriftUSD))
				report.Healthy = false
			}
		}

		report.TotalUSD += balUSD
		report.Venues = append(report.Venues, vr)
	}

	log.Printf("treasury: reconciliation total=$%.2f venues=%d healthy=%v",
		report.TotalUSD, len(report.Venues), report.Healthy)

	return report
}
