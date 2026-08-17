// -------------------------------------------------------------------------------
// TUI - Browser Model Tests
//
// Author: Alex Freidah
//
// Drives the Bubble Tea model with teatest against a fake lister: asserts the
// load/render round-trip, prefix navigation, and load-more pagination without
// touching the network.
// -------------------------------------------------------------------------------

package tui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// fakeLister returns canned listing pages keyed by "prefix|continuation" and
// canned location ledgers keyed by object key.
type fakeLister struct {
	pages     map[string]*adminapi.ObjectListResponse
	locations map[string]*adminapi.ObjectLocationsResponse
	status    *adminapi.StatusResponse
	logs      *adminapi.LogsResponse
	replic    *adminapi.ReplicationStatusResponse
	workers   *adminapi.WorkersResponse
	cleanupQ  *adminapi.CleanupQueueResponse
	cleanupD  *adminapi.CleanupDLQResponse
	cacheStat *adminapi.CacheStatsResponse
	requeued  *adminapi.CleanupDLQRequeueResponse
	scrubbed  *adminapi.ScrubKeyResponse
	scrubErr  error               // when set, ScrubKey fails
	opEvents  []adminstream.Event // canned RunOp stream (nil = single ok result)
	opErr     error               // when set, RunOp fails to open
	opRequest opsRequest          // the request the last action resolved to

	drainStarted   string                            // backend passed to StartDrain
	drainCancelled string                            // backend passed to CancelDrain
	drainProgress  []*adminapi.DrainProgressResponse // readings served in order
	drainErr       error                             // when set, every drain call fails
	reconciled     string                            // backend passed to ReconcileBackend
	reconcileResp  *adminapi.ReconcileResponse       // canned reconcile counts
	reconcileErr   error                             // when set, ReconcileBackend fails

	flatPages     map[string]*adminapi.ObjectListResponse // flat listings keyed by prefix
	flatErr       error                                   // when set, ListObjectsFlat fails
	payload       []byte                                  // body DownloadObject serves
	downloaded    string                                  // key passed to DownloadObject
	downloadErr   error                                   // when set, DownloadObject fails
	uploaded      string                                  // key passed to UploadObject
	uploadedBytes []byte                                  // body UploadObject received
	uploadedSize  int64                                   // size UploadObject was told
	uploadErr     error                                   // when set, UploadObject fails
	deletedKey    string                                  // key passed to DeleteObject
	deletedPrefix string                                  // prefix passed to DeletePrefix
	deletedCount  int                                     // count DeletePrefix reports
	deleteErr     error                                   // when set, both deletes fail
}

func (f *fakeLister) ListObjects(_ context.Context, prefix, continuation string) (*adminapi.ObjectListResponse, error) {
	if p, ok := f.pages[prefix+"|"+continuation]; ok {
		return p, nil
	}
	return &adminapi.ObjectListResponse{}, nil
}

func (f *fakeLister) GetObjectLocations(_ context.Context, key string) (*adminapi.ObjectLocationsResponse, error) {
	if r, ok := f.locations[key]; ok {
		return r, nil
	}
	return &adminapi.ObjectLocationsResponse{Key: key}, nil
}

func (f *fakeLister) ScrubKey(_ context.Context, key string) (*adminapi.ScrubKeyResponse, error) {
	if f.scrubErr != nil {
		return nil, f.scrubErr
	}
	if f.scrubbed != nil {
		return f.scrubbed, nil
	}
	return &adminapi.ScrubKeyResponse{Key: key}, nil
}

func (f *fakeLister) GetStatus(_ context.Context) (*adminapi.StatusResponse, error) {
	if f.status != nil {
		return f.status, nil
	}
	return &adminapi.StatusResponse{}, nil
}

func (f *fakeLister) GetLogs(_ context.Context, _ string) (*adminapi.LogsResponse, error) {
	if f.logs != nil {
		return f.logs, nil
	}
	return &adminapi.LogsResponse{}, nil
}

func (f *fakeLister) GetReplicationStatus(_ context.Context) (*adminapi.ReplicationStatusResponse, error) {
	if f.replic != nil {
		return f.replic, nil
	}
	return &adminapi.ReplicationStatusResponse{}, nil
}

func (f *fakeLister) GetWorkers(_ context.Context) (*adminapi.WorkersResponse, error) {
	if f.workers != nil {
		return f.workers, nil
	}
	return &adminapi.WorkersResponse{}, nil
}

func (f *fakeLister) GetCleanupQueue(_ context.Context) (*adminapi.CleanupQueueResponse, error) {
	if f.cleanupQ != nil {
		return f.cleanupQ, nil
	}
	return &adminapi.CleanupQueueResponse{}, nil
}

func (f *fakeLister) GetCleanupDLQ(_ context.Context) (*adminapi.CleanupDLQResponse, error) {
	if f.cleanupD != nil {
		return f.cleanupD, nil
	}
	return &adminapi.CleanupDLQResponse{}, nil
}

func (f *fakeLister) GetCacheStats(_ context.Context) (*adminapi.CacheStatsResponse, error) {
	if f.cacheStat != nil {
		return f.cacheStat, nil
	}
	return &adminapi.CacheStatsResponse{}, nil
}

func (f *fakeLister) RequeueCleanupDLQ(_ context.Context, backend string) (*adminapi.CleanupDLQRequeueResponse, error) {
	if f.requeued != nil {
		return f.requeued, nil
	}
	return &adminapi.CleanupDLQRequeueResponse{Backend: backend}, nil
}

// ListObjectsFlat serves the canned flat page, for the key counting a prefix
// delete does before it confirms.
func (f *fakeLister) ListObjectsFlat(_ context.Context, prefix, _ string) (*adminapi.ObjectListResponse, error) {
	if f.flatErr != nil {
		return nil, f.flatErr
	}
	if p, ok := f.flatPages[prefix]; ok {
		return p, nil
	}
	return &adminapi.ObjectListResponse{}, nil
}

// DownloadObject serves the canned payload as the object body.
func (f *fakeLister) DownloadObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	if f.downloadErr != nil {
		return nil, 0, f.downloadErr
	}
	f.downloaded = key
	return io.NopCloser(bytes.NewReader(f.payload)), int64(len(f.payload)), nil
}

// UploadObject drains the body and records what was stored where.
func (f *fakeLister) UploadObject(_ context.Context, key string, body io.Reader, size int64) error {
	if f.uploadErr != nil {
		return f.uploadErr
	}
	stored, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.uploaded, f.uploadedBytes, f.uploadedSize = key, stored, size
	return nil
}

// DeleteObject records the key removed.
func (f *fakeLister) DeleteObject(_ context.Context, key string) (*adminapi.ObjectDeleteResponse, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.deletedKey = key
	return &adminapi.ObjectDeleteResponse{Deleted: 1}, nil
}

// DeletePrefix records the prefix removed and reports the canned count.
func (f *fakeLister) DeletePrefix(_ context.Context, prefix string) (*adminapi.ObjectDeleteResponse, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.deletedPrefix = prefix
	return &adminapi.ObjectDeleteResponse{Deleted: f.deletedCount}, nil
}

// StartDrain records the backend asked to drain and reports f.drainErr.
func (f *fakeLister) StartDrain(_ context.Context, backend string) (*adminapi.BackendOperationResponse, error) {
	f.drainStarted = backend
	if f.drainErr != nil {
		return nil, f.drainErr
	}
	return &adminapi.BackendOperationResponse{Backend: backend}, nil
}

// DrainProgress serves the canned readings in order, repeating the last one
// once they run out, so a poll loop settles instead of running dry.
func (f *fakeLister) DrainProgress(_ context.Context, _ string) (*adminapi.DrainProgressResponse, error) {
	if f.drainErr != nil {
		return nil, f.drainErr
	}
	if len(f.drainProgress) == 0 {
		return &adminapi.DrainProgressResponse{}, nil
	}
	next := f.drainProgress[0]
	if len(f.drainProgress) > 1 {
		f.drainProgress = f.drainProgress[1:]
	}
	return next, nil
}

// CancelDrain records the backend whose drain was cancelled.
func (f *fakeLister) CancelDrain(_ context.Context, backend string) (*adminapi.BackendOperationResponse, error) {
	f.drainCancelled = backend
	if f.drainErr != nil {
		return nil, f.drainErr
	}
	return &adminapi.BackendOperationResponse{Backend: backend}, nil
}

// ReconcileBackend records the backend reconciled and reports canned counts.
func (f *fakeLister) ReconcileBackend(_ context.Context, backend string) (*adminapi.ReconcileResponse, error) {
	f.reconciled = backend
	if f.reconcileErr != nil {
		return nil, f.reconcileErr
	}
	if f.reconcileResp != nil {
		return f.reconcileResp, nil
	}
	return &adminapi.ReconcileResponse{}, nil
}

// RunOp returns a stream over the canned events (or a single ok result when
// none are set), or f.opErr when configured to fail. The request is recorded
// so a test can assert what the action resolved to.
func (f *fakeLister) RunOp(_ context.Context, _ *opsAction, req opsRequest) (adminclient.EventStream, error) {
	f.opRequest = req
	if f.opErr != nil {
		return nil, f.opErr
	}
	events := f.opEvents
	if events == nil {
		events = []adminstream.Event{{Kind: adminstream.KindResult, Outcome: adminstream.OutcomeOK, Message: "ok"}}
	}
	return adminclient.NewSliceStream(events...), nil
}

// waitForText fails the test unless the given text appears in the output.
func waitForText(t *testing.T, tm *teatest.TestModel, text string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte(text))
	}, teatest.WithDuration(3*time.Second))
}

// TestBrowser_LoadsAndDescends covers the initial load and descending into a
// directory: the child page's object appears and the prefix advances.
func TestBrowser_LoadsAndDescends(t *testing.T) {
	f := &fakeLister{pages: map[string]*adminapi.ObjectListResponse{
		"|":        {CommonPrefixes: []string{"photos/"}, Objects: []adminapi.ObjectEntry{{Key: "readme", Size: 10}}},
		"photos/|": {Objects: []adminapi.ObjectEntry{{Key: "photos/sunset", Size: 5}}},
	}}
	tm := teatest.NewTestModel(t, initialModel(f), teatest.WithInitialTermSize(80, 24))

	waitForText(t, tm, "photos")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // descend into the highlighted photos/
	waitForText(t, tm, "sunset")

	tm.Type("q")
	fm, ok := tm.FinalModel(t).(*model)
	if !ok {
		t.Fatal("final model is not a model")
	}
	if fm.prefix != "photos/" {
		t.Errorf("prefix = %q, want photos/", fm.prefix)
	}
}

// TestBrowser_HumanSizesAndFilter covers the human-readable SIZE column and the
// interactive filter: typing narrows the listing and the status line reports the
// match count.
func TestBrowser_HumanSizesAndFilter(t *testing.T) {
	f := &fakeLister{pages: map[string]*adminapi.ObjectListResponse{
		"|": {Objects: []adminapi.ObjectEntry{
			{Key: "alpha", Size: 2048},
			{Key: "beta", Size: 5},
			{Key: "gamma", Size: 10},
		}},
	}}
	tm := teatest.NewTestModel(t, initialModel(f), teatest.WithInitialTermSize(80, 24))

	waitForText(t, tm, "2.0 KiB") // alpha's size rendered in IEC units
	tm.Type("/")                  // focus the filter
	tm.Type("bet")                // narrow to beta
	waitForText(t, tm, "1 of 3 shown")

	tm.Send(tea.KeyMsg{Type: tea.KeyEsc}) // abandon the filter
	tm.Type("q")
	fm, ok := tm.FinalModel(t).(*model)
	if !ok {
		t.Fatal("final model is not a model")
	}
	if fm.filter.Value() != "" || len(fm.visible) != 3 {
		t.Errorf("after esc: filter=%q visible=%d", fm.filter.Value(), len(fm.visible))
	}
}

// TestBrowser_InspectsObject covers opening the inspector on a leaf object: the
// copy ledger loads, its backends render, and esc returns to the listing.
func TestBrowser_InspectsObject(t *testing.T) {
	f := &fakeLister{
		pages: map[string]*adminapi.ObjectListResponse{
			"|": {Objects: []adminapi.ObjectEntry{{Key: "readme", Size: 10}}},
		},
		locations: map[string]*adminapi.ObjectLocationsResponse{
			"readme": {Key: "readme", Locations: []adminapi.ObjectLocation{
				{Backend: "minio-a", SizeBytes: 10},
				{Backend: "minio-c", SizeBytes: 10},
			}},
		},
	}
	tm := teatest.NewTestModel(t, initialModel(f), teatest.WithInitialTermSize(80, 24))

	waitForText(t, tm, "readme")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // open the inspector on readme
	waitForText(t, tm, "minio-c")
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc}) // back to the listing
	waitForText(t, tm, "readme")

	tm.Type("q")
	fm, ok := tm.FinalModel(t).(*model)
	if !ok {
		t.Fatal("final model is not a model")
	}
	if fm.mode != modeBrowse {
		t.Errorf("mode = %v, want browse after esc", fm.mode)
	}
}

// TestBrowser_OpensBackendsView covers jumping to the backends section: the
// status snapshot loads, a backend row renders with its health, and esc returns
// focus to the sidebar without leaving the section.
func TestBrowser_OpensBackendsView(t *testing.T) {
	f := &fakeLister{
		pages: map[string]*adminapi.ObjectListResponse{
			"|": {Objects: []adminapi.ObjectEntry{{Key: "readme", Size: 10}}},
		},
		status: &adminapi.StatusResponse{
			DBHealthy:   true,
			UsagePeriod: "2026-07",
			Backends: []adminapi.BackendStatus{
				{Name: "minio-a", Healthy: true, BytesUsed: 2048, BytesLimit: 4096},
			},
		},
	}
	tm := teatest.NewTestModel(t, initialModel(f), teatest.WithInitialTermSize(120, 24))

	waitForText(t, tm, "readme")
	tm.Type("b")                          // jump to the backends section
	waitForText(t, tm, "minio-a")         // the loaded status snapshot rendered
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc}) // back to the sidebar

	tm.Type("q")
	fm, ok := tm.FinalModel(t).(*model)
	if !ok {
		t.Fatal("final model is not a model")
	}
	if fm.section != sectionBackends || !fm.navFocus {
		t.Errorf("section=%v navFocus=%v, want backends + focused sidebar", fm.section, fm.navFocus)
	}
}

// TestBrowser_OpensLogsView covers jumping to the logs section: the log page
// loads and an entry's message renders, and esc returns focus to the sidebar.
func TestBrowser_OpensLogsView(t *testing.T) {
	f := &fakeLister{
		pages: map[string]*adminapi.ObjectListResponse{
			"|": {Objects: []adminapi.ObjectEntry{{Key: "readme", Size: 10}}},
		},
		logs: &adminapi.LogsResponse{Entries: []adminapi.LogEntry{
			{Level: "INFO", Component: "replicator", Message: "copied-marker-xyz"},
		}},
	}
	tm := teatest.NewTestModel(t, initialModel(f), teatest.WithInitialTermSize(120, 24))

	waitForText(t, tm, "readme")
	tm.Type("l") // jump to the logs section
	waitForText(t, tm, "copied-marker-xyz")
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc}) // back to the sidebar

	tm.Type("q")
	fm, ok := tm.FinalModel(t).(*model)
	if !ok {
		t.Fatal("final model is not a model")
	}
	if fm.section != sectionLogs || !fm.navFocus {
		t.Errorf("section=%v navFocus=%v, want logs + focused sidebar", fm.section, fm.navFocus)
	}
}

// TestBrowser_OpensReplicationView covers jumping to the replication section:
// the snapshot loads, the pending counts render, and esc returns focus to the
// sidebar without leaving the section.
func TestBrowser_OpensReplicationView(t *testing.T) {
	f := &fakeLister{
		pages: map[string]*adminapi.ObjectListResponse{
			"|": {Objects: []adminapi.ObjectEntry{{Key: "readme", Size: 10}}},
		},
		replic: &adminapi.ReplicationStatusResponse{
			Factor: 3, UnderReplicated: 77, OverReplicated: 4, ComputedAt: time.Now(),
		},
	}
	tm := teatest.NewTestModel(t, initialModel(f), teatest.WithInitialTermSize(120, 24))

	waitForText(t, tm, "readme")
	tm.Type("p") // jump to the replication section
	// The snapshot-age line re-renders every auto-refresh tick, so it is the
	// reliable proof the stats (not the spinner) are on screen; the static
	// lines render once and get diffed away on later frames.
	waitForText(t, tm, "snapshot age")
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc}) // back to the sidebar

	tm.Type("q")
	fm, ok := tm.FinalModel(t).(*model)
	if !ok {
		t.Fatal("final model is not a model")
	}
	if fm.section != sectionReplication || !fm.navFocus {
		t.Errorf("section=%v navFocus=%v, want replication + focused sidebar", fm.section, fm.navFocus)
	}
	if fm.replication.snap == nil || fm.replication.snap.UnderReplicated != 77 {
		t.Errorf("snapshot = %+v, want loaded with 77 under-replicated", fm.replication.snap)
	}
	if stats := fm.replicationStats(); !strings.Contains(stats, "under-replicated") || !strings.Contains(stats, "77") {
		t.Errorf("stats = %q, want under-replicated + 77", stats)
	}
}

// TestBrowser_RunsOpsAction covers the ops section end to end: jump to it,
// select an action, confirm it, and watch the streamed result render.
func TestBrowser_RunsOpsAction(t *testing.T) {
	f := &fakeLister{
		pages: map[string]*adminapi.ObjectListResponse{
			"|": {Objects: []adminapi.ObjectEntry{{Key: "readme", Size: 10}}},
		},
		opEvents: []adminstream.Event{
			{Kind: adminstream.KindStart, Op: "rebalance"},
			{Kind: adminstream.KindResult, Outcome: adminstream.OutcomeOK, Message: "moved 7 objects"},
		},
	}
	tm := teatest.NewTestModel(t, initialModel(f), teatest.WithInitialTermSize(120, 24))

	waitForText(t, tm, "readme")
	tm.Type("o") // jump to the ops section
	waitForText(t, tm, "Rebalance backends")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // arm the first action's confirm
	waitForText(t, tm, "y/N")
	tm.Type("y") // accept and run
	waitForText(t, tm, "moved 7 objects")

	tm.Type("q")
	fm, ok := tm.FinalModel(t).(*model)
	if !ok {
		t.Fatal("final model is not a model")
	}
	if fm.section != sectionOps {
		t.Errorf("section = %v, want ops", fm.section)
	}
	if fm.ops.running {
		t.Error("ops still running after result, want finished")
	}
	if !strings.Contains(strings.Join(fm.ops.lines, "\n"), "moved 7 objects") {
		t.Errorf("ops output = %q, want the result line", fm.ops.lines)
	}
}

// TestBrowser_LoadMoreAppends covers pagination: a truncated first page plus a
// scroll to the bottom pulls the continuation page and appends its rows.
func TestBrowser_LoadMoreAppends(t *testing.T) {
	f := &fakeLister{pages: map[string]*adminapi.ObjectListResponse{
		"|":     {Objects: []adminapi.ObjectEntry{{Key: "alpha"}, {Key: "beta"}}, Truncated: true, Next: "beta"},
		"|beta": {Objects: []adminapi.ObjectEntry{{Key: "gamma"}}},
	}}
	tm := teatest.NewTestModel(t, initialModel(f), teatest.WithInitialTermSize(80, 24))

	waitForText(t, tm, "beta")
	tm.Send(tea.KeyMsg{Type: tea.KeyDown}) // reach the bottom row, triggering load-more
	waitForText(t, tm, "gamma")

	tm.Type("q")
	fm, ok := tm.FinalModel(t).(*model)
	if !ok {
		t.Fatal("final model is not a model")
	}
	if len(fm.entries) != 3 {
		t.Errorf("entries = %d, want 3 (alpha, beta, gamma)", len(fm.entries))
	}
}
