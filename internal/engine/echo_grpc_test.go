package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// echoAdapterDir is the committed echo-style adapter directory, relative to
// the engine package (internal/engine). Using the real adapter verifies the
// checked-in .proto/.desc/scripts are correct end to end.
const echoAdapterDir = "../../adapters/echo-style"

// loadEchoDescriptor reads and parses the committed echo.desc from the
// echo-style adapter directory.
func loadEchoDescriptor(t *testing.T) *descriptorpb.FileDescriptorSet {
	t.Helper()
	descPath := filepath.Join(echoAdapterDir, "schemas", "echo.desc")
	data, err := os.ReadFile(descPath)
	if err != nil {
		t.Fatalf("read %s: %v", descPath, err)
	}
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(data, fds); err != nil {
		t.Fatalf("unmarshal echo.desc: %v", err)
	}
	return fds
}

// TestEchoGRPCAdapter loads the committed echo-style adapter, serves it,
// and makes real dynamic gRPC client calls to every RPC, asserting correct
// behavior including stateful accumulation across calls.
func TestEchoGRPCAdapter(t *testing.T) {
	// Resolve the adapter directory to an absolute path so Load() works
	// regardless of the test working directory.
	absDir, err := filepath.Abs(echoAdapterDir)
	if err != nil {
		t.Fatalf("resolve adapter dir: %v", err)
	}
	fds := loadEchoDescriptor(t)

	// Build the engine with the echo-style adapter.
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"echo": {Adapter: absDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	ctx := context.Background()
	_, cancel, err := e.ServeForTest(ctx)
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(30 * time.Millisecond)

	target := e.GrpcTarget("echo")
	if target == "" {
		t.Fatal("no gRPC target for echo service")
	}

	// --- Say: echoes the message and returns echo_count ---
	resp1 := invokeGRPC(t, fds, target, "/stunt.example.Echo/Say",
		map[string]any{"message": "hello"})
	if got := resp1["message"]; got != "hello" {
		t.Errorf("Say message = %v, want %q", got, "hello")
	}
	if got := asInt(resp1["echoCount"]); got != 1 {
		t.Errorf("Say echoCount (1st) = %v, want 1", resp1["echoCount"])
	}

	resp2 := invokeGRPC(t, fds, target, "/stunt.example.Echo/Say",
		map[string]any{"message": "world"})
	if got := resp2["message"]; got != "world" {
		t.Errorf("Say message = %v, want %q", got, "world")
	}
	if got := asInt(resp2["echoCount"]); got != 2 {
		t.Errorf("Say echoCount (2nd) = %v, want 2", resp2["echoCount"])
	}

	// --- Add: accumulates values (stateful) ---
	add1 := invokeGRPC(t, fds, target, "/stunt.example.Echo/Add",
		map[string]any{"value": 10})
	if got := asInt(add1["total"]); got != 10 {
		t.Errorf("Add total (1st) = %v, want 10", add1["total"])
	}
	if got := asInt(add1["count"]); got != 1 {
		t.Errorf("Add count (1st) = %v, want 1", add1["count"])
	}

	add2 := invokeGRPC(t, fds, target, "/stunt.example.Echo/Add",
		map[string]any{"value": 5})
	if got := asInt(add2["total"]); got != 15 {
		t.Errorf("Add total (2nd) = %v, want 15", add2["total"])
	}
	if got := asInt(add2["count"]); got != 2 {
		t.Errorf("Add count (2nd) = %v, want 2", add2["count"])
	}

	// --- ListEchoes: reflects prior Say calls (stateful) ---
	listResp := invokeGRPC(t, fds, target, "/stunt.example.Echo/ListEchoes",
		map[string]any{})
	echoes, ok := listResp["echoes"].([]any)
	if !ok {
		t.Fatalf("ListEchoes echoes is %T, want []any", listResp["echoes"])
	}
	if len(echoes) != 2 {
		t.Fatalf("ListEchoes len = %d, want 2", len(echoes))
	}
	// Verify the recorded messages match the Say calls.
	msg0 := echoes[0].(map[string]any)["message"]
	msg1 := echoes[1].(map[string]any)["message"]
	if msg0 != "hello" || msg1 != "world" {
		t.Errorf("ListEchoes messages = [%v, %v], want [hello, world]", msg0, msg1)
	}
}

// asInt normalises a JSON-decoded numeric value (float64 or json.Number) to
// an int for comparison. protojson marshals int32 fields as JSON numbers,
// which json.Unmarshal converts to float64.
func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}
