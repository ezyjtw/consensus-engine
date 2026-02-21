package consensus

import "math"

func ComputeMid(q Quote) float64 {
	return (q.BestBid + q.BestAsk) / 2.0
}

func ComputeExecutableBuy(q Quote, notionalUSD float64, slippageBps, depthPenaltyBps float64) (float64, bool) {
	if q.Orderbook != nil && len(q.Orderbook.Asks) > 0 {
		return vwapBuy(q.Orderbook.Asks, notionalUSD, depthPenaltyBps)
	}
	buf := math.Max(SpreadBps(q.BestBid, q.BestAsk)*0.5, slippageBps)
	return q.BestAsk * (1 + buf/10000.0), true
}

func ComputeExecutableSell(q Quote, notionalUSD float64, slippageBps, depthPenaltyBps float64) (float64, bool) {
	if q.Orderbook != nil && len(q.Orderbook.Bids) > 0 {
		return vwapSell(q.Orderbook.Bids, notionalUSD, depthPenaltyBps)
	}
	buf := math.Max(SpreadBps(q.BestBid, q.BestAsk)*0.5, slippageBps)
	return q.BestBid * (1 - buf/10000.0), true
}

func vwapBuy(asks [][2]float64, notionalUSD, depthPenaltyBps float64) (float64, bool) {
	remaining := notionalUSD
	totalCost := 0.0
	totalSize := 0.0
	for _, level := range asks {
		price, size := level[0], level[1]
		levelNotional := price * size
		if levelNotional >= remaining {
			partial := remaining / price
			totalCost += partial * price
			totalSize += partial
			remaining = 0
			break
		}
		totalCost += levelNotional
		totalSize += size
		remaining -= levelNotional
	}
	if remaining > 0 || totalSize < epsilon {
		lastAsk := asks[len(asks)-1][0]
		return lastAsk * (1 + depthPenaltyBps/10000.0), false
	}
	return totalCost / totalSize, true
}

func vwapSell(bids [][2]float64, notionalUSD, depthPenaltyBps float64) (float64, bool) {
	remaining := notionalUSD
	totalProceeds := 0.0
	totalSize := 0.0
	for _, level := range bids {
		price, size := level[0], level[1]
		levelNotional := price * size
		if levelNotional >= remaining {
			partial := remaining / price
			totalProceeds += partial * price
			totalSize += partial
			remaining = 0
			break
		}
		totalProceeds += levelNotional
		totalSize += size
		remaining -= levelNotional
	}
	if remaining > 0 || totalSize < epsilon {
		lastBid := bids[len(bids)-1][0]
		return lastBid * (1 - depthPenaltyBps/10000.0), false
	}
	return totalProceeds / totalSize, true
}

func ApplyFeesBuy(price, feeBps float64) float64 {
	return price * (1 + feeBps/10000.0)
}

func ApplyFeesSell(price, feeBps float64) float64 {
	return price * (1 - feeBps/10000.0)
}
