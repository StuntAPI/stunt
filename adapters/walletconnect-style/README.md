# WalletConnect-style adapter

A stunt adapter for simulating the **WalletConnect v2 relay** locally. All
data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by WalletConnect. "WalletConnect" and related marks are
> trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A deterministic, stateful mock of the WalletConnect v2 relay's dApp-facing
pairing + session surface. The **key value** is auto-pairing and auto-approving:
a dApp can test its full WalletConnect integration **without a second device or
QR scan**.

- Create a pairing from a `wc:` URI or auto-generate one.
- Propose a session with required namespaces.
- Approve the session (simulated wallet approval) to get accounts + methods.
- Send wallet JSON-RPC requests (`eth_requestAccounts`, `personal_sign`,
  `eth_sendTransaction`) and get synthetic responses.
- List, extend, and disconnect sessions.

### Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/v1/pairings` | Establish a pairing (from `wc:` URI or auto) |
| POST | `/v1/sessions` | Propose a session |
| GET | `/v1/sessions` | List active sessions |
| POST | `/v1/sessions/{topic}/approve` | Approve (acknowledge) a session |
| POST | `/v1/sessions/{topic}/request` | Send a wallet JSON-RPC request |
| POST | `/v1/sessions/{topic}/extend` | Refresh session expiry |
| DELETE | `/v1/sessions/{topic}` | Disconnect a session |

### Auto-approve behavior

When a session is approved, the mock wallet returns these namespaces:

```json
{
  "eip155": {
    "accounts": ["eip155:1:0x1234...5678"],
    "methods": ["eth_sendTransaction", "personal_sign"],
    "events": ["chainChanged", "accountsChanged"]
  }
}
```

Session requests are auto-approved:
- `eth_requestAccounts` → `[<wallet_address>]`
- `personal_sign` → synthetic signature hash
- `eth_sendTransaction` → synthetic tx hash

## Usage

```yaml
services:
  walletconnect:
    adapter: ./adapters/walletconnect-style
```

Then `stunt up` and point your WC client at the served address.
