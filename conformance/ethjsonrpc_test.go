package conformance

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// TestEthJSONRPCClientConformance drives go-ethereum's ethclient — the
// canonical Go client for JSON-RPC chains — against the eth-jsonrpc-style
// adapter: dial, chain id, block number, seeded balances, code, and
// headers through the client's own RPC layer (hex-quantity encoding,
// block-tag semantics, and result decoding are all exercised client-side).
func TestEthJSONRPCClientConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "eth-jsonrpc-style")
	client, err := ethclient.Dial(base)
	if err != nil {
		t.Fatalf("ethclient.Dial: %v", err)
	}
	defer client.Close()

	// ===== chain id + latest block number =====
	chainID, err := client.ChainID(ctx)
	if err != nil {
		t.Fatalf("ChainID: %v", err)
	}
	if chainID.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("chain id = %v, want 1", chainID)
	}
	bn, err := client.BlockNumber(ctx)
	if err != nil {
		t.Fatalf("BlockNumber: %v", err)
	}
	if bn != 0 {
		t.Fatalf("block number = %d, want 0 (genesis, no txs sent)", bn)
	}
	Record(t, "go-ethereum", "eth-jsonrpc-style", "ethclient.ChainID + BlockNumber")

	// ===== seeded balances decode as hex-quantity wei =====
	bal, err := client.BalanceAt(ctx, common.HexToAddress("0x0000000000000000000000000000000000000000"), nil)
	if err != nil {
		t.Fatalf("BalanceAt: %v", err)
	}
	if bal.Cmp(new(big.Int).Exp(big.NewInt(10), big.NewInt(21), nil)) != 0 {
		t.Fatalf("zero-address balance = %v wei, want 1e21 (1000 ETH)", bal)
	}
	Record(t, "go-ethereum", "eth-jsonrpc-style", "BalanceAt decodes seeded hex-quantity wei")

	// ===== accounts hold no bytecode =====
	code, err := client.CodeAt(ctx, common.HexToAddress("0x0000000000000000000000000000000000000000"), nil)
	if err != nil {
		t.Fatalf("CodeAt: %v", err)
	}
	if len(code) != 0 {
		t.Fatalf("CodeAt = %#x, want empty (no EVM execution)", code)
	}
	Record(t, "go-ethereum", "eth-jsonrpc-style", "CodeAt returns empty (no EVM execution)")

	// ===== genesis header through the block API =====
	header, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		t.Fatalf("HeaderByNumber: %v", err)
	}
	if header.Number.Cmp(big.NewInt(0)) != 0 {
		t.Fatalf("latest header number = %v, want 0", header.Number)
	}
	Record(t, "go-ethereum", "eth-jsonrpc-style", "HeaderByNumber returns the genesis header")

	// ===== nonce for an untouched account =====
	nonce, err := client.PendingNonceAt(ctx, common.HexToAddress("0x0000000000000000000000000000000000000001"))
	if err != nil {
		t.Fatalf("PendingNonceAt: %v", err)
	}
	if nonce != 0 {
		t.Fatalf("nonce = %d, want 0", nonce)
	}
	Record(t, "go-ethereum", "eth-jsonrpc-style", "PendingNonceAt for an untouched account")
}
