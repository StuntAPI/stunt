# Ethereum JSON-RPC adapter

A stunt adapter for simulating an **Ethereum JSON-RPC provider** (Alchemy /
Infura / QuickNode shape) locally. All data is synthetic — no real API data
is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by any Ethereum JSON-RPC provider. "Ethereum" and related marks
> are trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A deterministic, stateful mock of an Ethereum JSON-RPC provider's HTTP surface.
It does **not** execute real EVM bytecode — it simulates the JSON-RPC responses
deterministically so that:

- A transaction sent via `eth_sendRawTransaction` shows up in a receipt.
- The block number advances predictably (each transaction mines one block).
- Logs are retrievable via `eth_getLogs` and `eth_getTransactionReceipt`.
- The same input always produces the same output (no faucet hell, no rate limits).

### JSON-RPC 2.0

All methods are served at a single `POST /` endpoint. Both single requests
(`{jsonrpc, method, params, id}`) and **batch requests** (`[{...},{...}]`) are
supported — batch responses are returned as arrays.

### Methods

| Method | Result | Notes |
|--------|--------|-------|
| `eth_chainId` | `"0x1"` | Mainnet chain ID |
| `eth_blockNumber` | `"0xN"` | Advances by 1 per `eth_sendRawTransaction` |
| `eth_gasPrice` | `"0x3b9aca00"` | 1 Gwei |
| `eth_getBalance` | hex wei | Seeded accounts |
| `eth_getTransactionCount` | hex nonce | Advances after send |
| `eth_sendRawTransaction` | tx hash | Mints hash, bumps block, records tx + logs |
| `eth_getTransactionReceipt` | receipt obj | Status `"0x1"`, logs, block info |
| `eth_getTransactionByHash` | tx obj | Stateful |
| `eth_call` | hex | Deterministic by calldata (selector echo) |
| `eth_getLogs` | `[log]` | Filtered by block range, address, topics |
| `eth_getBlockByNumber` | block obj | With transactions (full or hashes) |
| `eth_getBlockByHash` | block obj | |
| `eth_estimateGas` | `"0x5208"` | 21000 gas |
| `eth_getCode` | `"0x"` | Empty (no bytecode execution) |
| `eth_feeHistory` | obj | Base fee per gas |
| `web3_clientVersion` | `"Stunt/v1.0.0/mock"` | |
| `net_version` | `"1"` | Mainnet |
| `net_listening` | `true` | |
| Unknown method | error | Code `-32601` |

## Determinism & State

| Store | Purpose |
|-------|---------|
| `transactions` | All sent transactions |
| `receipts` | Receipts for sent transactions (statusful) |
| `logs` | Event logs from sent transactions |
| `blocks` | Genesis + mined blocks |

KV is used for the block number counter, nonce map, and balance map. The chain
is seeded with a genesis block (block 0) and synthetic accounts with balances.

Transaction hashes are deterministic: the same raw transaction hex always
produces the same hash (via a pseudo-keccak function — not real keccak256, but
deterministic and collision-resistant enough for local testing).

## Usage

```yaml
services:
  eth:
    adapter: ./adapters/eth-jsonrpc-style
```

Then `stunt up` and point your web3 client (ethers.js, web3.py, viem) at the
served address as the JSON-RPC URL.
