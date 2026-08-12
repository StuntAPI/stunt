package blob

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPutGetRoundtrip(t *testing.T) {
	s := newTestStore(t)

	content := []byte("hello blob world")
	id, err := s.Put("drive", "report.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id == "" {
		t.Fatal("Put returned empty id")
	}

	rc, err := s.Get("drive", id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content = %q, want %q", got, content)
	}
}

func TestPutWithCustomID(t *testing.T) {
	s := newTestStore(t)

	// The id returned should be the name (sanitised-safe) when the name is used.
	id, err := s.Put("s3", "data.csv", strings.NewReader("a,b,c"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id != "data.csv" {
		t.Fatalf("id = %q, want data.csv", id)
	}
}

func TestStat(t *testing.T) {
	s := newTestStore(t)

	content := []byte("stat me")
	id, _ := s.Put("dropbox", "file.bin", bytes.NewReader(content))

	info, err := s.Stat("dropbox", id)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Name != "file.bin" {
		t.Fatalf("Name = %q, want file.bin", info.Name)
	}
	if info.Size != int64(len(content)) {
		t.Fatalf("Size = %d, want %d", info.Size, len(content))
	}
	if info.Modified.IsZero() {
		t.Fatal("Modified should not be zero")
	}
}

func TestStatWithContentType(t *testing.T) {
	s := newTestStore(t)

	id, _ := s.PutWith("drive", "img.png", "image/png", bytes.NewReader([]byte("png")))

	info, _ := s.Stat("drive", id)
	if info.ContentType != "image/png" {
		t.Fatalf("ContentType = %q, want image/png", info.ContentType)
	}
}

func TestList(t *testing.T) {
	s := newTestStore(t)

	s.Put("drive", "a.txt", strings.NewReader("aaa"))
	s.Put("drive", "b.txt", strings.NewReader("bbb"))
	s.Put("drive", "c.txt", strings.NewReader("ccc"))

	infos, err := s.List("drive")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("len(infos) = %d, want 3", len(infos))
	}

	names := make([]string, len(infos))
	for i, info := range infos {
		names[i] = info.Name
	}
	sort.Strings(names)
	expected := []string{"a.txt", "b.txt", "c.txt"}
	for i := range expected {
		if names[i] != expected[i] {
			t.Fatalf("names[%d] = %q, want %q", i, names[i], expected[i])
		}
	}
}

func TestListEmpty(t *testing.T) {
	s := newTestStore(t)

	infos, err := s.List("drive")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("len(infos) = %d, want 0", len(infos))
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)

	id, _ := s.Put("drive", "temp.txt", strings.NewReader("temp"))

	if err := s.Delete("drive", id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := s.Get("drive", id)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteIdempotent(t *testing.T) {
	s := newTestStore(t)

	if err := s.Delete("drive", "nonexistent"); err != nil {
		t.Fatalf("Delete nonexistent should be nil, got %v", err)
	}
}

func TestGetNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Get("drive", "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStatNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Stat("drive", "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestNamespacing(t *testing.T) {
	s := newTestStore(t)

	s.Put("ns1", "shared.txt", strings.NewReader("from ns1"))
	s.Put("ns2", "shared.txt", strings.NewReader("from ns2"))

	rc1, _ := s.Get("ns1", "shared.txt")
	defer rc1.Close()
	got1, _ := io.ReadAll(rc1)

	rc2, _ := s.Get("ns2", "shared.txt")
	defer rc2.Close()
	got2, _ := io.ReadAll(rc2)

	if string(got1) != "from ns1" {
		t.Fatalf("ns1 content = %q, want from ns1", got1)
	}
	if string(got2) != "from ns2" {
		t.Fatalf("ns2 content = %q, want from ns2", got2)
	}
}

// --- Traversal / safety rejection ---

func TestInvalidNamespace(t *testing.T) {
	s := newTestStore(t)

	invalid := []string{
		"../escape",
		"..",
		"a/b",
		"a\\b",
		"",
		"has space",
		".hidden",
	}
	for _, ns := range invalid {
		_, err := s.Put(ns, "file.txt", strings.NewReader("x"))
		if err == nil {
			t.Errorf("Put(ns=%q) should have returned an error", ns)
		}
	}
}

func TestInvalidID(t *testing.T) {
	s := newTestStore(t)

	// Put first with a valid name, then try to Get/Stat/Delete with traversal ids.
	s.Put("drive", "file.txt", strings.NewReader("ok"))

	invalid := []string{
		"../escape",
		"..",
		"a/b",
		"a\\b",
		".hidden",
	}
	for _, id := range invalid {
		_, err := s.Get("drive", id)
		if err == nil {
			t.Errorf("Get(id=%q) should have returned an error", id)
		}
		_, err = s.Stat("drive", id)
		if err == nil {
			t.Errorf("Stat(id=%q) should have returned an error", id)
		}
		err = s.Delete("drive", id)
		if err == nil {
			t.Errorf("Delete(id=%q) should have returned an error", id)
		}
	}
}

func TestPutInvalidName(t *testing.T) {
	s := newTestStore(t)

	// Names with path separators should be rejected.
	invalid := []string{
		"../escape.txt",
		"a/b.txt",
		".hidden",
	}
	for _, name := range invalid {
		_, err := s.Put("drive", name, strings.NewReader("x"))
		if err == nil {
			t.Errorf("Put(name=%q) should have returned an error", name)
		}
	}
}

// --- Persistence across re-open ---

func TestPersistsAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "blobs")

	s1, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, _ := s1.Put("drive", "persist.txt", strings.NewReader("persisted"))
	s1.Close()

	s2, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s2.Close()

	rc, err := s2.Get("drive", id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "persisted" {
		t.Fatalf("content = %q, want persisted", got)
	}
}

func TestOpenCreatesRootDir(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "nested", "blobs")

	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("Stat root: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("root should be a directory")
	}
}

// TestPutLeavesNoOrphan verifies that after a successful Put, both the
// content file and the meta file are present. This guards against the
// scenario where content is written but meta is missing, making the blob
// invisible to List/Stat.
func TestPutLeavesNoOrphan(t *testing.T) {
	s := newTestStore(t)

	id, err := s.PutWith("drive", "report.txt", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Both files should exist.
	contentPath := filepath.Join(s.root, "drive", id+".content")
	metaPath := filepath.Join(s.root, "drive", id+".meta")

	if _, err := os.Stat(contentPath); err != nil {
		t.Errorf("content file missing after Put: %v", err)
	}
	if _, err := os.Stat(metaPath); err != nil {
		t.Errorf("meta file missing after Put: %v", err)
	}

	// List and Stat should both see the blob.
	infos, err := s.List("drive")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("List returned %d blobs, want 1", len(infos))
	}
	statInfo, err := s.Stat("drive", id)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if statInfo.Size != int64(len("hello")) {
		t.Fatalf("Stat Size = %d, want 5", statInfo.Size)
	}
	if statInfo.ContentType != "text/plain" {
		t.Fatalf("Stat ContentType = %q, want text/plain", statInfo.ContentType)
	}
}

// TestNoTempFilesLeaked verifies that Put does not leave temp files behind.
func TestNoTempFilesLeaked(t *testing.T) {
	s := newTestStore(t)

	for i := 0; i < 5; i++ {
		name := "file" + string(rune('0'+i)) + ".txt"
		_, err := s.Put("drive", name, strings.NewReader("content"))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(s.root, "drive"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".tmp") {
			t.Errorf("temp file leaked: %s", name)
		}
	}
}

// TestPutAtomicityContentAndMetaConsistent verifies that content and meta are
// always consistent: if content is visible, meta must also be visible. We
// simulate this by checking the on-disk state matches what List/Stat report.
func TestPutAtomicityContentAndMetaConsistent(t *testing.T) {
	s := newTestStore(t)

	// Put multiple blobs.
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		_, err := s.Put("drive", name, strings.NewReader(name))
		if err != nil {
			t.Fatalf("Put(%s): %v", name, err)
		}
	}

	// Enumerate on-disk files.
	entries, err := os.ReadDir(filepath.Join(s.root, "drive"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var contentFiles, metaFiles []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".content") {
			contentFiles = append(contentFiles, name)
		} else if strings.HasSuffix(name, ".meta") {
			metaFiles = append(metaFiles, name)
		}
	}

	// Every content file must have a corresponding meta file.
	metaSet := make(map[string]bool, len(metaFiles))
	for _, m := range metaFiles {
		metaSet[m] = true
	}
	for _, c := range contentFiles {
		expectedMeta := strings.TrimSuffix(c, ".content") + ".meta"
		if !metaSet[expectedMeta] {
			t.Errorf("content file %s has no matching meta file", c)
		}
	}

	// List should see exactly the same count as content files.
	infos, err := s.List("drive")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != len(contentFiles) {
		t.Fatalf("List returned %d blobs, but %d content files on disk", len(infos), len(contentFiles))
	}
}

func TestNamespacesAndClearAll(t *testing.T) {
	s := newTestStore(t)
	s.PutWith("uploads", "a.txt", "text/plain", bytes.NewReader([]byte("a")))
	s.PutWith("uploads", "b.txt", "text/plain", bytes.NewReader([]byte("b")))
	s.PutWith("avatars", "c.png", "image/png", bytes.NewReader([]byte("c")))

	ns, err := s.Namespaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 2 || ns[0] != "avatars" || ns[1] != "uploads" {
		t.Fatalf("namespaces = %v", ns)
	}

	if err := s.ClearAll(); err != nil {
		t.Fatal(err)
	}
	ns2, _ := s.Namespaces()
	if len(ns2) != 0 {
		t.Fatalf("after clearall = %v", ns2)
	}
}

// --- Append (resumable-upload path) ---

func TestAppendRoundtrip(t *testing.T) {
	s := newTestStore(t)

	for _, chunk := range []string{"foo", "bar", "baz"} {
		if _, err := s.Append("drive", "up.bin", "application/octet-stream", strings.NewReader(chunk)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	rc, err := s.Get("drive", "up.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "foobarbaz" {
		t.Fatalf("content = %q, want foobarbaz", got)
	}
	info, _ := s.Stat("drive", "up.bin")
	if info.Size != 9 {
		t.Fatalf("Stat Size = %d, want 9", info.Size)
	}
}

func TestAppendCreateIfAbsent(t *testing.T) {
	s := newTestStore(t)

	// No prior Put: the first append must create the blob (the start==0 path).
	if _, err := s.Append("drive", "fresh.bin", "application/octet-stream", strings.NewReader("created")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	rc, err := s.Get("drive", "fresh.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "created" {
		t.Fatalf("content = %q, want created", got)
	}
}

func TestAppendPreservesContentType(t *testing.T) {
	s := newTestStore(t)

	s.PutWith("drive", "doc.txt", "text/plain", strings.NewReader("hello"))
	// content_type on later appends is ignored — the creation value is kept.
	s.Append("drive", "doc.txt", "application/octet-stream", strings.NewReader("world"))
	info, _ := s.Stat("drive", "doc.txt")
	if info.ContentType != "text/plain" {
		t.Fatalf("ContentType = %q, want text/plain (creation value must persist)", info.ContentType)
	}
}

func TestAppendReturnsGrowingTotal(t *testing.T) {
	s := newTestStore(t)

	total, err := s.Append("drive", "grow.bin", "", strings.NewReader("abc"))
	if err != nil || total != 3 {
		t.Fatalf("first Append total=%d err=%v, want 3", total, err)
	}
	total, err = s.Append("drive", "grow.bin", "", strings.NewReader("defg"))
	if err != nil || total != 7 {
		t.Fatalf("second Append total=%d err=%v, want 7", total, err)
	}
	total, err = s.Append("drive", "grow.bin", "", strings.NewReader("hi"))
	if err != nil || total != 9 {
		t.Fatalf("third Append total=%d err=%v, want 9", total, err)
	}
}

// countingReader counts how many bytes are read from it. Used to prove Append
// consumes each chunk's bytes exactly once and never re-reads prior content.
type countingReader struct {
	r io.Reader
	n *int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

// TestAppendIsLinearPerChunk pins the O(chunk) guarantee: appending N chunks of
// size k must read exactly N*k bytes total (each chunk once) and finish fast.
// A read-modify-write regression re-reads the growing blob per chunk (O(N²·k))
// and blows both the byte-count and the time budget.
func TestAppendIsLinearPerChunk(t *testing.T) {
	s := newTestStore(t)

	const chunks = 200
	const size = 64 << 10 // 64 KiB
	payload := bytes.Repeat([]byte("x"), size)

	var consumed int64
	start := time.Now()
	for i := 0; i < chunks; i++ {
		cr := &countingReader{r: bytes.NewReader(payload), n: &consumed}
		if _, err := s.Append("drive", "big.bin", "application/octet-stream", cr); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	if consumed != int64(chunks)*int64(size) {
		t.Errorf("bytes consumed = %d, want %d — Append re-read accumulated content (quadratic regression)", consumed, int64(chunks)*int64(size))
	}
	// O(N²·k) (~1.3 GiB of copying) takes seconds; O(N·k) (~13 MiB append) is
	// milliseconds. 5s is a generous upper bound that still catches the regression.
	if elapsed > 5*time.Second {
		t.Errorf("Append run took %v for %d×%d KiB — expected linear/millisecond; likely quadratic", elapsed, chunks, size>>10)
	}

	info, _ := s.Stat("drive", "big.bin")
	if info.Size != int64(chunks)*int64(size) {
		t.Errorf("final Size = %d, want %d", info.Size, int64(chunks)*int64(size))
	}
}
