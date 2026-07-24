package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Instance records one running stunt server in the global registry
// (~/.stunt/instances.json). It is the cross-manifest counterpart to the
// per-manifest RuntimeFile (which stays as the fast path for stunt down /
// auto-discovery).
type Instance struct {
	PID            int      `json:"pid"`
	Manifest       string   `json:"manifest"`
	Mode           string   `json:"mode"`
	Services       []string `json:"services,omitempty"`
	Addresses      []string `json:"addresses,omitempty"`
	DashboardURL   string   `json:"dashboard_url,omitempty"`
	DashboardToken string   `json:"dashboard_token,omitempty"`
	StartedAt      string   `json:"started_at"`
}

// instancesFile is the on-disk shape of the registry.
type instancesFile struct {
	Instances []Instance `json:"instances"`
}

// Registry is a file-locked view over ~/.stunt/instances.json. All mutations
// go through a flock-protected read-modify-write + atomic rename; reads
// optionally prune dead PIDs (signal-0 liveness check, same as stunt down).
type Registry struct {
	path string
}

// instancesRegistryPath returns ~/.stunt/instances.json (created lazily).
func instancesRegistryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home dir: %w", err)
	}
	dir := filepath.Join(home, ".stunt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return filepath.Join(dir, "instances.json"), nil
}

// OpenRegistry opens (creating ~/.stunt if needed) the global instance registry.
func OpenRegistry() (*Registry, error) {
	p, err := instancesRegistryPath()
	if err != nil {
		return nil, err
	}
	return &Registry{path: p}, nil
}

// withLock runs fn while holding an exclusive flock on the registry file's
// companion lock file. The lock is held for the whole RMW so concurrent
// stunt up / stunt down can't clobber each other. On Windows flock is a
// no-op (atomic rename still guards against torn reads; lost updates are
// healed by PID-pruning on the next List).
func (r *Registry) withLock(fn func() error) error {
	if runtime.GOOS == "windows" {
		return fn()
	}
	lockPath := r.path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer lf.Close()
	if err := unix.Flock(int(lf.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("flock %s: %w", lockPath, err)
	}
	defer unix.Flock(int(lf.Fd()), unix.LOCK_UN)
	return fn()
}

// read loads the registry file (or an empty set if it doesn't exist yet).
// Caller must already hold the lock.
func (r *Registry) read() (instancesFile, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return instancesFile{}, nil
		}
		return instancesFile{}, fmt.Errorf("read %s: %w", r.path, err)
	}
	var f instancesFile
	if err := json.Unmarshal(data, &f); err != nil {
		return instancesFile{}, fmt.Errorf("parse %s: %w", r.path, err)
	}
	return f, nil
}

// write atomically writes the registry (temp file + rename in the same dir).
// Caller must already hold the lock.
func (r *Registry) write(f instancesFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal instances: %w", err)
	}
	dir := filepath.Dir(r.path)
	tmp, err := os.CreateTemp(dir, ".instances-*.json")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, r.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename into %s: %w", r.path, err)
	}
	return nil
}

// Register adds inst (replacing any entry with the same PID). Flock-protected.
func (r *Registry) Register(inst Instance) error {
	return r.withLock(func() error {
		f, err := r.read()
		if err != nil {
			return err
		}
		// Replace any existing entry for this PID.
		out := f.Instances[:0]
		for _, i := range f.Instances {
			if i.PID != inst.PID {
				out = append(out, i)
			}
		}
		out = append(out, inst)
		f.Instances = out
		return r.write(f)
	})
}

// Deregister removes the entry for pid (best-effort; missing is fine).
func (r *Registry) Deregister(pid int) error {
	return r.withLock(func() error {
		f, err := r.read()
		if err != nil {
			return err
		}
		out := f.Instances[:0]
		for _, i := range f.Instances {
			if i.PID != pid {
				out = append(out, i)
			}
		}
		f.Instances = out
		return r.write(f)
	})
}

// List returns the registered instances. If prune is true, dead-PID entries
// (signal-0 liveness check) are dropped and the pruned file is rewritten.
// Results are sorted by StartedAt (oldest first).
func (r *Registry) List(prune bool) ([]Instance, error) {
	var out []Instance
	err := r.withLock(func() error {
		f, err := r.read()
		if err != nil {
			return err
		}
		if prune {
			kept := f.Instances[:0]
			for _, i := range f.Instances {
				if pidAlive(i.PID) {
					kept = append(kept, i)
				}
			}
			f.Instances = kept
			if werr := r.write(f); werr != nil {
				return werr
			}
		}
		out = f.Instances
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(a, b int) bool {
		return out[a].StartedAt < out[b].StartedAt
	})
	return out, nil
}

// pidAlive reports whether pid is currently running (signal-0 liveness check).
// Mirrors stunt down's check. On Windows, signal 0 isn't supported by the
// syscall package; we optimistically treat the PID as alive (pruning is a
// best-effort nicety, not a correctness gate).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// age returns a human-friendly duration string since the RFC3339 startedAt.
func age(startedAt string) string {
	t, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return "?"
	}
	d := time.Since(t).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}
