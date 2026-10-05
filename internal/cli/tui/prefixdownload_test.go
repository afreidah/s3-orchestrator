// -------------------------------------------------------------------------------
// TUI - Prefix Download Tests
//
// Author: Alex Freidah
//
// Covers downloading everything under a prefix: the tree is written with the
// key layout beneath the prefix across listing pages, a failure part way or a
// key that cannot be a local path leaves nothing behind, an existing
// destination is refused, and the progress line counts objects as well as
// bytes.
// -------------------------------------------------------------------------------

package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// treeClient serves a paged flat listing and a body per key, and fails the
// download of one key when asked to.
type treeClient struct {
	*fakeLister
	pages  []*adminapi.ObjectListResponse
	bodies map[string]string
	failOn string

	mu      sync.Mutex
	fetched []string
}

// ListObjectsFlat serves the page the continuation token names: "" is the
// first page, and each page's Next is the index of the following one.
func (c *treeClient) ListObjectsFlat(_ context.Context, _, continuation string) (*adminapi.ObjectListResponse, error) {
	i := 0
	if continuation != "" {
		i = int(continuation[0] - '0')
	}
	return c.pages[i], nil
}

// DownloadObject serves the key's body, or fails for the key set in failOn.
func (c *treeClient) DownloadObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	if key == c.failOn {
		return nil, 0, errors.New("provider said no")
	}
	c.mu.Lock()
	c.fetched = append(c.fetched, key)
	c.mu.Unlock()
	body := c.bodies[key]
	return io.NopCloser(bytes.NewReader([]byte(body))), int64(len(body)), nil
}

// photoTree is two listing pages under bucket/photos/: two objects in a
// nested layout and a folder marker, which holds no bytes.
func photoTree() *treeClient {
	return &treeClient{
		fakeLister: &fakeLister{},
		pages: []*adminapi.ObjectListResponse{
			{Objects: []adminapi.ObjectEntry{{Key: "bucket/photos/a.jpg", Size: 3}}, Truncated: true, Next: "1"},
			{Objects: []adminapi.ObjectEntry{{Key: "bucket/photos/2024/"}, {Key: "bucket/photos/2024/b.jpg", Size: 5}}},
		},
		bodies: map[string]string{"bucket/photos/a.jpg": "aaa", "bucket/photos/2024/b.jpg": "bbbbb"},
	}
}

// downloadPhotos drives a prefix download of bucket/photos/ through the pane
// into dest and returns the finished message.
func downloadPhotos(t *testing.T, c *treeClient, dest string) (*model, transferDoneMsg) {
	t.Helper()
	m := filesModel(t, c.fakeLister)
	m.client = c
	m.table.SetCursor(0)
	m.handleBrowseKey(key("D"))
	submit(t, m, dest)
	done := settle(t, m)
	m.applyTransferDone(done)
	return m, done
}

// assertNoLeftovers fails when a temporary download directory is left in dir.
func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".part-") {
			t.Errorf("left %s behind", e.Name())
		}
	}
}

// TestPrefixDownload_WritesTheTree verifies every object across both listing
// pages lands at its path beneath the destination, folder markers are
// skipped, and the result names the object count.
func TestPrefixDownload_WritesTheTree(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dest := filepath.Join(parent, "photos")
	m, done := downloadPhotos(t, photoTree(), dest)

	if done.err != nil {
		t.Fatalf("download failed: %v", done.err)
	}
	for path, want := range map[string]string{"a.jpg": "aaa", "2024/b.jpg": "bbbbb"} {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(path)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	assertNoLeftovers(t, parent)
	if m.status == nil || !m.status.ok || !strings.Contains(m.status.text, "2 objects") {
		t.Errorf("status = %+v, want a success naming 2 objects", m.status)
	}
}

// TestPrefixDownload_FailureLeavesNothing verifies an object that fails part
// way through leaves neither the destination nor a temporary directory, and
// the error names the key and how far the download got.
func TestPrefixDownload_FailureLeavesNothing(t *testing.T) {
	t.Parallel()
	c := photoTree()
	c.failOn = "bucket/photos/2024/b.jpg"
	parent := t.TempDir()
	dest := filepath.Join(parent, "photos")
	m, done := downloadPhotos(t, c, dest)

	if done.err == nil || !strings.Contains(done.err.Error(), "2024/b.jpg") || !strings.Contains(done.err.Error(), "1 of 2") {
		t.Errorf("err = %v, want one naming the key and the progress", done.err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("destination exists after a failed download: %v", err)
	}
	assertNoLeftovers(t, parent)
	if m.status == nil || m.status.ok {
		t.Errorf("status = %+v, want a failure", m.status)
	}
}

// TestPrefixDownload_Refusals verifies the cases that fail before any object
// is fetched: an existing destination, a key that cannot be a local path, and
// a prefix with nothing under it.
func TestPrefixDownload_Refusals(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		client  func() *treeClient
		prepare func(dest string)
		want    string
	}{
		"existing destination": {
			client:  photoTree,
			prepare: func(dest string) { _ = os.Mkdir(dest, 0o750) },
			want:    "already exists",
		},
		"unsafe key": {
			client: func() *treeClient {
				c := photoTree()
				c.pages = []*adminapi.ObjectListResponse{{Objects: []adminapi.ObjectEntry{{Key: "bucket/photos/../escape", Size: 1}}}}
				return c
			},
			want: "cannot be written as a local path",
		},
		"empty prefix": {
			client: func() *treeClient {
				c := photoTree()
				c.pages = []*adminapi.ObjectListResponse{{}}
				return c
			},
			want: "nothing to download",
		},
	}
	for name, tc := range cases {
		parent := t.TempDir()
		dest := filepath.Join(parent, "photos")
		if tc.prepare != nil {
			tc.prepare(dest)
		}
		c := tc.client()
		_, done := downloadPhotos(t, c, dest)
		if done.err == nil || !strings.Contains(done.err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, done.err, tc.want)
		}
		if len(c.fetched) != 0 {
			t.Errorf("%s: fetched %v before refusing", name, c.fetched)
		}
		assertNoLeftovers(t, parent)
	}
}

// failingLister fails every listing.
type failingLister struct{ *treeClient }

// ListObjectsFlat always fails.
func (failingLister) ListObjectsFlat(context.Context, string, string) (*adminapi.ObjectListResponse, error) {
	return nil, errors.New("listing refused")
}

// brokenBody fails part way through being read.
type brokenBody struct{ *treeClient }

// DownloadObject serves a body that breaks after its first byte.
func (brokenBody) DownloadObject(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(io.MultiReader(strings.NewReader("a"), errReader{})), 3, nil
}

// errReader fails every read.
type errReader struct{}

// Read always fails.
func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// TestPrefixDownload_ListAndReadErrors verifies a listing that fails and a
// body that breaks mid-stream both fail the download and leave nothing.
func TestPrefixDownload_ListAndReadErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		client adminClient
		want   string
	}{
		"listing fails": {client: failingLister{photoTree()}, want: "listing refused"},
		"body breaks":   {client: brokenBody{photoTree()}, want: "connection reset"},
	}
	for name, tc := range cases {
		parent := t.TempDir()
		dest := filepath.Join(parent, "photos")
		tr := startPrefixDownload(tc.client, "bucket/photos/", dest)
		m := filesModel(t, &fakeLister{})
		m.files.transfer = tr
		done := settle(t, m)
		if done.err == nil || !strings.Contains(done.err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, done.err, tc.want)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Errorf("%s: destination exists: %v", name, err)
		}
		assertNoLeftovers(t, parent)
	}
}

// TestTransferLine_Prefix verifies a prefix download says it is listing until
// it knows the object count, then reports objects and bytes.
func TestTransferLine_Prefix(t *testing.T) {
	t.Parallel()
	tr := &transfer{kind: transferPrefix, key: "bucket/photos/"}
	if got := transferLine(tr); !strings.Contains(got, "listing") {
		t.Errorf("before the listing = %q, want listing", got)
	}
	tr.objects.Store(340)
	tr.fetched.Store(12)
	tr.total.Store(4096)
	tr.moved.Store(1024)
	got := transferLine(tr)
	for _, want := range []string{"12 / 340 objects", "25%"} {
		if !strings.Contains(got, want) {
			t.Errorf("line = %q, want %q", got, want)
		}
	}
}
