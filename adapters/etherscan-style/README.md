# Etherscan-style adapter

A stunt adapter for simulating an **Etherscan-style block explorer API**
locally. All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Etherscan or any block explorer service. "Etherscan" and
> related marks are trademarks of their respective owners. See
> [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A behavioral mock of the Etherscan API v1 surface — the REST API used by
explorers (Etherscan, Arbiscan, BscScan, Polygonscan, etc.) for account
lookups, contract verification, and chain stats.

### Auth

Auth is via the `apikey` query parameter. The mock accepts any non-empty
value. A missing `apikey` returns `{status: "0", message: "Missing API key"}`.

### Modules & Actions

All requests go to `GET /api?apikey=...&module=...&action=...`.

| Module | Action | Result | Notes |
|--------|--------|--------|-------|
| `account` | `balance` | wei string | Balance for a single address |
| `account` | `balancemulti` | `[{account, balance}]` | Multiple addresses (comma-separated) |
| `account` | `txlist` | `[{hash, from, to, value, ...}]` | Transaction history |
| `contract` | `getabi` | ABI JSON string | Verified contract ABI |
| `contract` | `getsourcecode` | `[{ContractName, CompilerVersion, ABI, SourceCode}]` | Source + ABI |
| `token` | `tokenholderlist` | `[{TokenHolderAddress, TokenHolderQuantity}]` | Token holders |
| `stats` | `ethsupply` | wei string | Total ETH supply |
| `stats` | `ethprice` | `{ethusd, ethbtc, ...}` | ETH price |
| `transaction` | `getstatus` | `"1"` | Tx status |
| `block` | `getblockreward` | `{blockNumber, blockMiner, blockReward}` | Block reward |

### Response envelope

All responses use the exact Etherscan envelope:

```json
{ "status": "1", "message": "OK", "result": <data> }
```

Where `result` is often a **string** (numbers as strings) or an **array**.
Errors use `status: "0"` and `message: "NOTOK"`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `accounts` | Seeded synthetic addresses with balances |
| `transactions` | Seeded synthetic transactions |
| `contracts` | Seeded synthetic contract ABIs + source |
| `token_holders` | Seeded synthetic token holders |

## Usage

```yaml
services:
  etherscan:
    adapter: ./adapters/etherscan-style
```

Then `stunt up` and point your Etherscan SDK / client at the served address.
