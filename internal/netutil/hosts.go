package netutil

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultHostsFile is the system hosts file path. Tests should use temp
// files instead.
const DefaultHostsFile = "/etc/hosts"

// Managed-block markers. Everything between these markers (inclusive) is
// owned by stunt and replaced on each sync.
const (
	beginMarker = "# BEGIN stunt"
	endMarker   = "# END stunt"
)

// HostEntry is a single line in the managed hosts block. It maps the host
// to the loopback address 127.0.0.1.
type HostEntry struct {
	Host string
}

// SyncHosts idempotently writes a managed block to the hosts file at path,
// with one "127.0.0.1 <host>" line per entry. If a managed block already
// exists it is replaced in place. Non-stunt content before and after the
// block is preserved. Entries are de-duplicated and sorted for deterministic
// output. If entries is empty no block is written (any existing block is
// removed).
func SyncHosts(path string, entries []HostEntry) error {
	hosts := dedupHostEntries(entries)
	for _, h := range hosts {
		if err := validateHost(h); err != nil {
			return fmt.Errorf("netutil: invalid host %q: %w", h, err)
		}
	}
	lines := buildHostLines(hosts)
	return writeManagedBlock(path, lines)
}

// SpoofHosts idempotently writes a managed block to the hosts file at path,
// redirecting real hostnames to the given IP addresses. It uses the same
// managed block as SyncHosts; calling both will replace the block with the
// latest set. If services is empty no block is written (any existing block
// is removed).
func SpoofHosts(path string, services map[string]string) error {
	for host, ip := range services {
		if err := validateHost(host); err != nil {
			return fmt.Errorf("netutil: invalid host %q: %w", host, err)
		}
		if err := validateIP(ip); err != nil {
			return fmt.Errorf("netutil: invalid IP for host %q: %w", host, err)
		}
	}
	lines := buildSpoofLines(services)
	return writeManagedBlock(path, lines)
}

// CleanHosts removes the managed block from the hosts file at path,
// preserving all other content. If no managed block exists the file is left
// unchanged. If the file does not exist this is a no-op.
func CleanHosts(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("netutil: read hosts file: %w", err)
	}

	cleaned := removeManagedBlock(string(content))
	if cleaned == string(content) {
		return nil // no block found, nothing to do
	}

	return atomicWriteFile(path, []byte(cleaned), 0o644)
}

// --- validation (C1/M3: prevent newline/whitespace injection) ---

// validateHost rejects a hostname that is empty or contains any whitespace
// (including newlines). This prevents injection of content outside the
// managed block via crafted hostnames.
func validateHost(h string) error {
	if h == "" {
		return errors.New("hostname is empty")
	}
	if strings.ContainsAny(h, " \t\r\n") {
		return fmt.Errorf("hostname contains whitespace or newlines")
	}
	return nil
}

// validateIP rejects an IP address that is empty or contains whitespace or
// newlines. This prevents injection in the SpoofHosts path.
func validateIP(ip string) error {
	if ip == "" {
		return errors.New("IP address is empty")
	}
	if strings.ContainsAny(ip, " \t\r\n") {
		return fmt.Errorf("IP address contains whitespace or newlines")
	}
	return nil
}

// --- internal ---

// atomicWriteFile writes data to a temp file in the same directory as path,
// then renames it to path. This is atomic on POSIX, preventing a partial
// write from corrupting the hosts file (I2).
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".stunt-hosts-*")
	if err != nil {
		return fmt.Errorf("netutil: create temp file: %w", err)
	}
	tmpPath := f.Name()

	// Best-effort cleanup if anything below fails.
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("netutil: write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("netutil: sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("netutil: close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		cleanup()
		return fmt.Errorf("netutil: chmod temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("netutil: rename temp file: %w", err)
	}
	return nil
}

// writeManagedBlock reads the file at path (creating if absent), removes any
// existing managed block, and inserts a fresh block with the given lines.
// If lines is empty, no block is inserted (effectively a clean). The write
// is atomic (temp file + rename) to prevent corruption on crash (I2).
func writeManagedBlock(path string, lines []string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("netutil: read hosts file: %w", err)
		}
		content = nil
	}

	cleaned := removeManagedBlock(string(content))

	// Build the new content: existing content (trimmed) + managed block.
	cleaned = strings.TrimSpace(cleaned)
	var buf bytes.Buffer
	if cleaned != "" {
		buf.WriteString(cleaned)
		buf.WriteByte('\n')
	}
	if len(lines) > 0 {
		buf.WriteString(beginMarker)
		buf.WriteByte('\n')
		for _, l := range lines {
			buf.WriteString(l)
			buf.WriteByte('\n')
		}
		buf.WriteString(endMarker)
		buf.WriteByte('\n')
	}

	return atomicWriteFile(path, buf.Bytes(), 0o644)
}

// removeManagedBlock strips the stunt-managed block (including markers)
// from content. Everything before beginMarker and after endMarker is kept.
func removeManagedBlock(content string) string {
	beginIdx := strings.Index(content, beginMarker)
	if beginIdx == -1 {
		return content // no block
	}

	afterBegin := content[beginIdx:]
	endOffset := strings.Index(afterBegin, endMarker)
	if endOffset == -1 {
		// Malformed: begin marker without end. Remove from begin to EOF.
		return strings.TrimSpace(content[:beginIdx])
	}

	// End of the end-marker line (include the trailing newline).
	lineEnd := beginIdx + endOffset + len(endMarker)
	if lineEnd < len(content) && content[lineEnd] == '\n' {
		lineEnd++
	}

	before := content[:beginIdx]
	after := content[lineEnd:]
	return before + after
}

// buildHostLines builds sorted, de-duplicated "127.0.0.1 <host>" lines.
func buildHostLines(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	sort.Strings(hosts)
	lines := make([]string, len(hosts))
	for i, h := range hosts {
		lines[i] = fmt.Sprintf("127.0.0.1 %s", h)
	}
	return lines
}

// buildSpoofLines builds sorted "<ip> <hostname>" lines from a services map.
func buildSpoofLines(services map[string]string) []string {
	if len(services) == 0 {
		return nil
	}
	keys := make([]string, 0, len(services))
	for k := range services {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, host := range keys {
		lines[i] = fmt.Sprintf("%s %s", services[host], host)
	}
	return lines
}

// dedupHostEntries extracts host names from entries, de-duplicates, and
// returns a sorted slice.
func dedupHostEntries(entries []HostEntry) []string {
	seen := make(map[string]bool)
	var hosts []string
	for _, e := range entries {
		if e.Host != "" && !seen[e.Host] {
			seen[e.Host] = true
			hosts = append(hosts, e.Host)
		}
	}
	return hosts
}
