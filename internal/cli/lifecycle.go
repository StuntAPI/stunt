package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RuntimeFile records the state of a running `stunt up` process so that
// `stunt down` can find and stop it. It is written as JSON under
// <manifest-dir>/.stunt/runtime/up.json when `stunt up` starts and removed
// on clean shutdown.
type RuntimeFile struct {
	PID       int      `json:"pid"`
	Manifest  string   `json:"manifest"`
	Mode      string   `json:"mode"`
	Addresses []string `json:"addresses"`
	StartedAt string   `json:"started_at"`
}

// runtimeDir returns the directory for runtime files under the manifest dir.
func runtimeDir(manifestDir string) string {
	return filepath.Join(manifestDir, stuntSubdir, "runtime")
}

// runtimeFilePath returns the path to the up.json runtime file.
func runtimeFilePath(manifestDir string) string {
	return filepath.Join(runtimeDir(manifestDir), "up.json")
}

// writeRuntimeFile atomically writes the runtime file. It creates the
// runtime directory if needed.
func writeRuntimeFile(manifestDir string, rt RuntimeFile) error {
	dir := runtimeDir(manifestDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	data, err := json.MarshalIndent(rt, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal runtime file: %w", err)
	}
	return os.WriteFile(runtimeFilePath(manifestDir), data, 0o644)
}

// readRuntimeFile reads and parses the runtime file. Returns an error
// wrapping os.ErrNotExist if the file does not exist.
func readRuntimeFile(manifestDir string) (*RuntimeFile, error) {
	data, err := os.ReadFile(runtimeFilePath(manifestDir))
	if err != nil {
		return nil, err
	}
	var rt RuntimeFile
	if err := json.Unmarshal(data, &rt); err != nil {
		return nil, fmt.Errorf("parse runtime file: %w", err)
	}
	return &rt, nil
}

// removeRuntimeFile removes the runtime file (best-effort).
func removeRuntimeFile(manifestDir string) {
	_ = os.Remove(runtimeFilePath(manifestDir))
}
