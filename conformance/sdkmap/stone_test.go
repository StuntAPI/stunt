package sdkmap

import "testing"

// Fixture mirrors dropbox-api-spec shape: namespace + import + type decls
// interleaved with one route of each flavor (plain, deprecated, versioned,
// content-host style attrs).
const stoneFixture = `namespace files
    "This namespace contains endpoints and data types for basic file operations."

import async

struct Metadata
    name String

route list_folder (ListFolderArg, ListFolderResult, ListFolderError)
    "doc"

route create_folder (CreateFolderArg, FolderMetadata, CreateFolderError) deprecated

route create_folder:2 (CreateFolderArg, CreateFolderResult, CreateFolderError)

route copy_batch/check:2 (async.PollArg, RelocationBatchV2JobStatus, async.PollError)

route upload (UploadArg, FileMetadata, UploadError)
    "doc"

    attrs
        host = "content"
        style = "upload"

union UploadError
    path (WriteError)

union_closed AccessError
`

func TestParseStone(t *testing.T) {
	routes, err := ParseStone([]byte(stoneFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /2/files/list_folder",
		"POST /2/files/create_folder",
		"POST /2/files/create_folder_v2",
		"POST /2/files/copy_batch/check_v2",
		"POST /2/files/upload",
	}
	if len(routes) != len(want) {
		t.Fatalf("%d routes, want %d: %+v", len(routes), len(want), routes)
	}
	for i, r := range routes {
		if got := r.Method + " " + r.Path; got != want[i] {
			t.Errorf("route %d = %q, want %q", i, got, want[i])
		}
	}
}

func TestParseStoneGuards(t *testing.T) {
	cases := []struct{ name, body string }{
		{"no routes", "namespace files\n\nstruct Void\n"},
		{"route before namespace", "route list_folder (A, B, C)\n"},
	}
	for _, c := range cases {
		if _, err := ParseStone([]byte(c.body)); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}
