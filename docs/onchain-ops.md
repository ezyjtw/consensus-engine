# On-Chain Operations

## Overview

The on-chain infrastructure enables the consensus-engine to interact with EVM
blockchains for DEX trading, liquidation keeping, bridge monitoring, and treasury
yield management.

## Components

### Wallet Management (`internal/onchain/wallet.go`)

The `Wallet` type manages signing keys and nonce tracking:

- **Address management** — each wallet has a fixed address and tracks nonces per chain
- **Pending transaction tracking** — monitors in-flight transactions with configurable max-pending limit
- **Transaction status lifecycle** — PENDING → CONFIRMED/FAILED/DROPPED/REPLACED
- **Automatic purging** — removes confirmed/failed transactions after configurable cutoff

### Nonce Manager (`internal/onchain/wallet.go`)

The `NonceManager` provides high-throughput nonce management:

- **Gap detection** — when a transaction fails, its nonce is returned to a gap queue
- **Gap filling** — subsequent transactions use gap nonces before incrementing
- **Multi-wallet support** — manages nonces across multiple wallets

### Transaction Builder (`internal/onchain/wallet.go`)

The `TxBuilder` constructs unsigned transaction payloads:

- **EIP-1559 support** — builds both legacy and EIP-1559 transactions
- **Swap transactions** — constructs DEX swap calldata (Uniswap-compatible)
- **Approval transactions** — builds ERC-20 approve calldata
- **Per-chain configuration** — different gas settings per chain

### Transaction Simulator (`internal/onchain/wallet.go`)

The `TxSimulator` validates transactions before submission:

- **Dry-run execution** — simulates via `eth_call` or Tenderly
- **Gas estimation** — predicts actual gas usage
- **State change preview** — shows expected storage changes
- **Result caching** — recent simulations are cached

## Supported Chains

| Chain | ID | EIP-1559 |
|---|---|---|
| Ethereum | 1 | Yes |
| Arbitrum | 42161 | Yes |
| Optimism | 10 | Yes |
| Base | 8453 | Yes |
| Polygon | 137 | Yes |
| BSC | 56 | No |

## DEX Pool Indexer (`internal/dexindex/indexer.go`)

Tracks and normalises DEX pool states across protocols:

### Supported Protocols
- **Uniswap V2** — constant product (x*y=k)
- **Uniswap V3** — concentrated liquidity with tick ranges
- **Curve Stable** — StableSwap invariant for correlated assets
- **Balancer** — weighted pools

### Capabilities
- Pool state tracking with staleness detection
- Quote simulation using constant-product AMM math
- Multi-hop route finding
- Price impact estimation
- Gas cost integration
- TVL-ranked pool discovery

## Bridge Monitor (`internal/bridge/monitor.go`)

Monitors cross-chain transfers for safety and timing:

### Bridge Types
- **Optimistic** — 7-day challenge window (Optimism, Arbitrum)
- **ZK Proof** — instant after proof generation (zkSync, Scroll)
- **Canonical** — native L1↔L2 bridges
- **Third Party** — external bridges (Across, Stargate, LayerZero)

### Monitoring
- Transfer lifecycle tracking (PENDING → IN_CHALLENGE → FINALIZED)
- Challenge window countdown alerts (WINDOW_CLOSING when < 1 hour)
- Automatic expiry detection (WINDOW_EXPIRED)
- Volume and finalization time statistics

## Environment Variables

| Variable | Description |
|---|---|
| `WALLET_ADDRESS` | Hot wallet address |
| `ETH_RPC_URL` | Ethereum RPC endpoint |
| `ARB_RPC_URL` | Arbitrum RPC endpoint |
| `BASE_RPC_URL` | Base RPC endpoint |

## Security Considerations

1. **Private keys** — never stored in config files; loaded from environment or HSM
2. **Max pending transactions** — hard limit prevents nonce exhaustion
3. **Gas price caps** — configurable maximum gas price prevents overspending
4. **Transaction simulation** — all transactions simulated before submission
5. **Approval management** — ERC-20 approvals use exact amounts, not infinite
6. **Challenge window monitoring** — alerts before optimistic bridge windows close
