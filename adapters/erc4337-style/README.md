# ERC-4337 bundler-style adapter

A stunt adapter for simulating an **ERC-4337 Account Abstraction bundler**
(EntryPoint v0.7) locally. All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by the ERC-4337 project, any bundler provider, or the Ethereum
> Foundation. "ERC-4337" and related marks are trademarks of their respective
> owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for
> **local development and testing only**.

## What it simulates

A deterministic, stateful mock of an ERC-4337 bundler RPC. The **key value** is
that gas estimates are deterministic (they differ per real bundler), userOps are
validated and stored, and receipts are available for sent userOps.

- Get the supported EntryPoint address (v0.7).
- Estimate gas for a userOp (deterministic plausible values).
- Send a userOp (with full field validation) and get a hash back.
- Retrieve the receipt for a sent userOp.
- Retrieve the full userOp by hash.
- Use the mock paymaster to sign sponsorship data.

### Bundler RPC methods

| Method | Result | Notes |
|--------|--------|-------|
| `eth_supportedEntryPoints` | `["0x0000000071727De22E5E9d8BAf0edAc6f37da032"]` | v0.7 EntryPoint |
| `eth_estimateUserOperationGas` | `{preVerificationGas, verificationGasLimit, callGasLimit}` | Deterministic |
| `eth_sendUserOperation` | userOp hash | Validates fields, stores userOp + receipt |
| `eth_getUserOperationReceipt` | receipt obj | `{userOpHash, sender, nonce, success, actualGasCost, actualGasUsed, logs, receipt}` |
| `eth_getUserOperationByHash` | userOp obj | Returns stored userOp |

### UserOp fields (required)

All of these fields must be present in a `sendUserOperation` or
`estimateUserOperationGas` call:

| Field | Type |
|-------|------|
| `sender` | address |
| `nonce` | hex |
| `initCode` | hex |
| `callData` | hex |
| `callGasLimit` | hex |
| `verificationGasLimit` | hex |
| `preVerificationGas` | hex |
| `maxFeePerGas` | hex |
| `maxPriorityFeePerGas` | hex |
| `paymasterAndData` | hex |
| `signature` | hex |

### Paymaster

`POST /paymaster/sign` with a `{userOp}` body returns an updated
`paymasterAndData` with a synthetic paymaster signature.

## Usage

```yaml
services:
  bundler:
    adapter: ./adapters/erc4337-style
```

Then `stunt up` and point your AA SDK (Permissionless, ConnectKit, etc.) at the
served address as the bundler RPC URL.
