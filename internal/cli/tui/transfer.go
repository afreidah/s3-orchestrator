// -------------------------------------------------------------------------------
// TUI - Object Transfers
//
// Author: Alex Freidah
//
// Downloads and uploads move real bytes, so they run off the main loop while
// the pane polls shared counters and renders how far they have got. A failed
// transfer leaves nothing behind: a download writes to a temporary file beside
// its destination and only renames it into place once the whole body is on
// disk, so an interrupted run cannot be mistaken for a complete one. A prefix
// download does the same for a whole tree: every object lands in a temporary
// directory beside the destination, which is renamed into place only once
// every object has been written.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// transferPollInterval is how often the pane re-reads a running transfer's
// byte counter. Fast enough to look live, slow enough not to redraw on every
// buffer.
const transferPollInterval = 150 * time.Millisecond

// transferKind names the direction of a transfer, for the lines the pane
// renders about it.
type transferKind int

// transferDownload and transferUpload move one object; transferPrefix
// downloads every object under a prefix into a local directory tree.
const (
	transferDownload transferKind = iota
	transferUpload
	transferPrefix
)

// treeDirMode is the mode a prefix download's directories are created with.
const treeDirMode = 0o750

// errNothingUnderPrefix reports a prefix download with no objects to fetch.
var errNothingUnderPrefix = errors.New("nothing to download")

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// verb words the transfer for a progress or completion line.
func (k transferKind) verb() string {
	if k == transferUpload {
		return "uploading"
	}
	return "downloading"
}

// past words the transfer for the line reporting it finished. A prefix
// download is a download, so it reads the same.
func (k transferKind) past() string {
	if k == transferUpload {
		return "uploaded"
	}
	return "downloaded"
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// transfer is one in-flight transfer. The counters are read by the main loop
// while the transfer goroutine writes them, so all of them are atomic.
type transfer struct {
	kind    transferKind
	key     string       // object key or prefix being moved
	local   string       // local path being read or written
	total   atomic.Int64 // bytes expected, 0 while the size is unknown
	moved   atomic.Int64 // bytes moved so far
	objects atomic.Int64 // objects a prefix download holds, 0 until the listing is complete
	fetched atomic.Int64 // objects a prefix download has written
	done    atomic.Bool  // set once the transfer finished or failed
	err     error        // set before done, read only after done is true
	cancel  context.CancelFunc
}

// transferDoneMsg reports a finished transfer. objects is how many a prefix
// download wrote.
type transferDoneMsg struct {
	kind    transferKind
	key     string
	local   string
	moved   int64
	objects int64
	err     error
}

// transferTickMsg schedules the next progress poll.
type transferTickMsg struct{}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// transferTick schedules the next poll of the running transfer.
func transferTick() tea.Cmd {
	return tea.Tick(transferPollInterval, func(time.Time) tea.Msg { return transferTickMsg{} })
}

// startDownload begins writing one object to a local path and returns the
// transfer the pane polls. The work runs in a goroutine so the loop stays
// responsive while bytes move.
func startDownload(client adminClient, key, local string) *transfer {
	ctx, cancel := context.WithCancel(context.Background())
	t := &transfer{kind: transferDownload, key: key, local: local, cancel: cancel}

	go func() {
		defer cancel()
		body, size, err := client.DownloadObject(ctx, key)
		if err != nil {
			t.finish(err)
			return
		}
		defer body.Close()
		t.total.Store(size)
		t.finish(writeToFile(local, body, &t.moved))
	}()
	return t
}

// startPrefixDownload begins writing every object under prefix into a new
// local directory, mirroring the key layout beneath the prefix.
func startPrefixDownload(client adminClient, prefix, local string) *transfer {
	ctx, cancel := context.WithCancel(context.Background())
	t := &transfer{kind: transferPrefix, key: prefix, local: local, cancel: cancel}

	go func() {
		defer cancel()
		t.finish(downloadTree(ctx, client, prefix, local, t))
	}()
	return t
}

// downloadTree lists everything under prefix, writes each object into a
// temporary directory beside dest, and renames that directory to dest once
// every object is on disk. It refuses a dest that already exists, so the
// rename never merges into or replaces a directory the operator already had,
// and a failure removes the temporary directory, so nothing partial is left.
func downloadTree(ctx context.Context, client adminClient, prefix, dest string, t *transfer) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	objects, err := listAll(ctx, client, prefix)
	if err != nil {
		return err
	}
	if len(objects) == 0 {
		return errNothingUnderPrefix
	}
	paths := make([]string, len(objects))
	var total int64
	for i, o := range objects {
		if paths[i], err = treePath(prefix, o.Key); err != nil {
			return err
		}
		total += o.Size
	}
	t.total.Store(total)
	t.objects.Store(int64(len(objects)))

	tmp, err := os.MkdirTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".part-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // nothing left to remove once the rename succeeded

	for i, o := range objects {
		if err := fetchInto(ctx, client, o.Key, filepath.Join(tmp, paths[i]), &t.moved); err != nil {
			return fmt.Errorf("%s: %w (%d of %d written, none kept)", o.Key, err, t.fetched.Load(), len(objects))
		}
		t.fetched.Add(1)
	}
	if err := os.Chmod(tmp, treeDirMode); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// listAll pages through every object under prefix. Keys ending in "/" are
// folder markers some clients create to make an empty directory visible; they
// hold no bytes and have no file to become, so they are left out.
func listAll(ctx context.Context, client adminClient, prefix string) ([]adminapi.ObjectEntry, error) {
	var out []adminapi.ObjectEntry
	next := ""
	for {
		page, err := client.ListObjectsFlat(ctx, prefix, next)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Objects {
			if !strings.HasSuffix(o.Key, "/") {
				out = append(out, o)
			}
		}
		if !page.Truncated || page.Next == "" {
			return out, nil
		}
		next = page.Next
	}
}

// treePath turns an object key into its path beneath the download directory.
// A key whose remainder after the prefix is not a plain relative path - one
// with "..", an absolute path, or an empty segment - is refused, since writing
// it could land outside the directory or silently lose the object.
func treePath(prefix, key string) (string, error) {
	rel := strings.TrimPrefix(key, prefix)
	local, err := filepath.Localize(rel)
	if err != nil {
		return "", fmt.Errorf("%s cannot be written as a local path under %s", key, prefix)
	}
	return local, nil
}

// fetchInto downloads one object to path, creating its parent directories.
// The file sits inside the download's temporary directory, so it needs no
// temporary name of its own.
func fetchInto(ctx context.Context, client adminClient, key, path string, moved *atomic.Int64) error {
	if err := os.MkdirAll(filepath.Dir(path), treeDirMode); err != nil {
		return err
	}
	body, _, err := client.DownloadObject(ctx, key)
	if err != nil {
		return err
	}
	defer body.Close()
	file, err := os.Create(path) //nolint:gosec // G304: path is built by treePath under a temporary directory
	if err != nil {
		return err
	}
	if _, err := bufpool.Copy(file, &countingReader{r: body, moved: moved}); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// startUpload begins storing a local file under one object key.
func startUpload(client adminClient, key, local string) *transfer {
	ctx, cancel := context.WithCancel(context.Background())
	t := &transfer{kind: transferUpload, key: key, local: local, cancel: cancel}

	go func() {
		defer cancel()
		file, err := os.Open(local)
		if err != nil {
			t.finish(err)
			return
		}
		defer file.Close()

		info, err := file.Stat()
		if err != nil {
			t.finish(err)
			return
		}
		t.total.Store(info.Size())
		t.finish(client.UploadObject(ctx, key, &countingReader{r: file, moved: &t.moved}, info.Size()))
	}()
	return t
}

// finish records the outcome and marks the transfer complete. err is written
// before done so a reader that sees done can trust it.
func (t *transfer) finish(err error) {
	t.err = err
	t.done.Store(true)
}

// writeToFile streams body into a temporary file beside dest and renames it
// into place only once the whole body landed, so a failure leaves no partial
// file where a complete one is expected.
func writeToFile(dest string, body io.Reader, moved *atomic.Int64) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".part-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename succeeded
	}()

	if _, err := bufpool.Copy(tmp, &countingReader{r: body, moved: moved}); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

// countingReader counts the bytes read through it, so the main loop can report
// progress without the transfer having to publish anything itself.
type countingReader struct {
	r     io.Reader
	moved *atomic.Int64
}

// Read passes through and records how much moved.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.moved.Add(int64(n))
	return n, err
}

// transferLine renders a running transfer's progress, with a percentage when
// the total is known. A prefix download also counts objects, and says it is
// still listing until it knows how many there are.
func transferLine(t *transfer) string {
	moved, total := t.moved.Load(), t.total.Load()
	line := t.kind.verb() + " " + t.key + "   "
	if t.kind == transferPrefix {
		objects := t.objects.Load()
		if objects == 0 {
			return line + "listing..."
		}
		line += fmt.Sprintf("%s / %s objects   ", grouped(int(t.fetched.Load())), grouped(int(objects)))
	}
	if total <= 0 {
		return line + humanize.Bytes(moved)
	}
	return line + fmt.Sprintf("%s / %s (%d%%)", humanize.Bytes(moved), humanize.Bytes(total), percentOf(moved, total))
}

// percentOf reports how far a transfer has got, capped at 100 so a body longer
// than its declared length cannot render past done.
func percentOf(moved, total int64) int {
	if total <= 0 {
		return 0
	}
	pct := int(moved * 100 / total)
	return min(pct, 100)
}
