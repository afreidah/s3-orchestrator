// -------------------------------------------------------------------------------
// TUI - Object Transfers
//
// Author: Alex Freidah
//
// Downloads and uploads move real bytes, so they run off the main loop while
// the pane polls a shared counter and renders how far they have got. A failed
// transfer leaves nothing behind: a download writes to a temporary file beside
// its destination and only renames it into place once the whole body is on
// disk, so an interrupted run cannot be mistaken for a complete one.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
)

// transferPollInterval is how often the pane re-reads a running transfer's
// byte counter. Fast enough to look live, slow enough not to redraw on every
// buffer.
const transferPollInterval = 150 * time.Millisecond

// transferKind names the direction of a transfer, for the lines the pane
// renders about it.
type transferKind int

// transferDownload and transferUpload are the two directions.
const (
	transferDownload transferKind = iota
	transferUpload
)

// verb words the transfer for a progress or completion line.
func (k transferKind) verb() string {
	if k == transferUpload {
		return "uploading"
	}
	return "downloading"
}

// past words the transfer for the line reporting it finished.
func (k transferKind) past() string {
	if k == transferUpload {
		return "uploaded"
	}
	return "downloaded"
}

// transfer is one in-flight object transfer. done and moved are read by the
// main loop while the transfer goroutine writes them, so both are atomic.
type transfer struct {
	kind   transferKind
	key    string       // object key being moved
	local  string       // local path being read or written
	total  int64        // bytes expected, 0 when the size is unknown
	moved  atomic.Int64 // bytes moved so far
	done   atomic.Bool  // set once the transfer finished or failed
	err    error        // set before done, read only after done is true
	cancel context.CancelFunc
}

// transferDoneMsg reports a finished transfer.
type transferDoneMsg struct {
	kind  transferKind
	key   string
	local string
	moved int64
	err   error
}

// transferTickMsg schedules the next progress poll.
type transferTickMsg struct{}

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
		t.total = size
		t.finish(writeToFile(local, body, &t.moved))
	}()
	return t
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
		t.total = info.Size()
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
// the total is known.
func transferLine(t *transfer) string {
	moved := t.moved.Load()
	if t.total <= 0 {
		return fmt.Sprintf("%s %s   %s", t.kind.verb(), t.key, humanize.Bytes(moved))
	}
	return fmt.Sprintf("%s %s   %s / %s (%d%%)",
		t.kind.verb(), t.key, humanize.Bytes(moved), humanize.Bytes(t.total), percentOf(moved, t.total))
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
