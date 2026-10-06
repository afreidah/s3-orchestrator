// -------------------------------------------------------------------------------
// TUI - Ops View
//
// Author: Alex Freidah
//
// The admin instance-action pane: a menu of write operations, each armed with a
// y/N confirm and then run against the admin API as a streamed run (run.go),
// whose output replaces the menu until it is closed.
// Reached with "o"; "esc" steps back to the menu, then to the nav.
// -------------------------------------------------------------------------------

package tui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	tea "github.com/charmbracelet/bubbletea"
)

// pathCompressExisting and pathDecompressExisting are the admin endpoints the
// whole-fleet and bounded compression entries both post to.
const (
	pathCompressExisting   = "/admin/api/compress-existing"
	pathDecompressExisting = "/admin/api/decompress-existing"
	pathEncryptExisting    = "/admin/api/encrypt-existing"
	pathDecryptExisting    = "/admin/api/decrypt-existing"
	pathScrub              = "/admin/api/scrub"
	pathBackfillChecksums  = "/admin/api/backfill-checksums"
	pathReconcile          = "/admin/api/reconcile"
	pathCleanupDLQRequeue  = "/admin/api/cleanup-dlq/requeue"
)

// queryBackend restricts a pass to one backend's copies. The backends pane sets
// it for every action it opens; the ops pane leaves it unset, which is what the
// server reads as the whole fleet.
const queryBackend = "backend"

// opsAction is one selectable admin operation. result decodes the single JSON
// summary a short action answers with; the long-running actions leave it nil
// and stream NDJSON progress instead. An action that needs a value from the
// operator sets ask, and resolve turns the typed value into the request that
// carries it.
type opsAction struct {
	label   string
	method  string
	path    string
	result  oneShotDecoder
	confirm string
	ask     inputRequest
	resolve func(value string) opsRequest
}

// inputRequest is the question an action asks before it can run, and the hint
// shown in the empty field.
type inputRequest struct {
	question    string
	placeholder string
}

// opsRequest is where an action sends and what it carries: the path (which a
// value can extend), the query, and the request body.
type opsRequest struct {
	path  string
	query url.Values
	body  []byte
}

// opsActions lists the instance actions in menu order: the maintenance passes
// first, then cache control, then the encryption and compression transitions.
// Drain is intentionally excluded; it is a per-backend flow handled elsewhere.
func opsActions() []opsAction {
	actions := maintenanceActions()
	actions = append(actions, cacheActions()...)
	actions = append(actions, encryptionActions()...)
	return append(actions, compressionActions()...)
}

// backendActions lists what one backend can be asked to do on its own. The
// menu is the same machinery the ops pane renders, so each entry streams its
// progress; only the request differs, and only by the backend it names.
//
// Drain and its cancellation are deliberately absent. Both own a polling watch
// that renders above the backends table, which is a different lifecycle from
// an action that opens a stream and reads it to the end, so they stay the
// hotkeys they already are.
func backendActions() []opsAction {
	return []opsAction{
		post("Scrub (verify integrity)", pathScrub,
			"Scrub every copy on this backend to verify integrity?", nil),
		post("Backfill checksums", pathBackfillChecksums,
			"Backfill missing checksums for this backend's copies?", nil),
		post("Reconcile metadata", pathReconcile,
			"Reconcile metadata against this backend's storage?", nil),
		post("Requeue dead-lettered cleanups", pathCleanupDLQRequeue,
			"Requeue this backend's dead-lettered cleanup rows?", nil),
		post("Encrypt existing objects", pathEncryptExisting,
			"Read and rewrite every plaintext copy on this backend as ciphertext?", nil),
		post("Decrypt existing objects", pathDecryptExisting,
			"Read and rewrite every encrypted copy on this backend as plaintext?", nil),
		post("Compress existing objects", pathCompressExisting,
			"Read and rewrite every uncompressed copy on this backend as chunked zstd?", nil),
		post("Decompress existing objects", pathDecompressExisting,
			"Read and rewrite every compressed copy on this backend back to its stored bytes?", nil),
	}
}

// opsOption adjusts an action that is not a plain POST-and-run.
type opsOption func(*opsAction)

// post builds an action that triggers a pass at path. result is nil for the
// operations that stream their own progress instead of answering with one
// summary.
func post(label, path, confirm string, result oneShotDecoder, opts ...opsOption) opsAction {
	a := opsAction{
		label:   label,
		method:  http.MethodPost,
		path:    path,
		confirm: confirm,
		result:  result,
	}
	for _, opt := range opts {
		opt(&a)
	}
	return a
}

// asks attaches an input prompt, so the value the operator types resolves into
// the request that carries it.
func asks(question, placeholder string, resolve func(string) opsRequest) opsOption {
	return func(a *opsAction) {
		a.ask = inputRequest{question: question, placeholder: placeholder}
		a.resolve = resolve
	}
}

// deletes sends the action as a DELETE, for the endpoints that drop something
// rather than start a pass.
func deletes() opsOption {
	return func(a *opsAction) { a.method = http.MethodDelete }
}

// maintenanceActions are the passes that keep placement, copy counts and
// integrity where the configuration says they should be.
func maintenanceActions() []opsAction {
	return []opsAction{
		post("Rebalance backends", "/admin/api/rebalance",
			"Rebalance objects across backends?", nil),
		post("Replicate under-replicated objects", "/admin/api/replicate",
			"Create the missing copies of under-replicated objects?", nil),
		post("Clean over-replicated copies", "/admin/api/over-replication",
			"Remove over-replicated object copies?", nil),
		post("Scrub (verify integrity)", "/admin/api/scrub",
			"Scrub every object to verify integrity?", nil),
		post("Backfill checksums", "/admin/api/backfill-checksums",
			"Backfill missing object checksums?", nil),
		post("Purge unreadable copies", "/admin/api/unreadable",
			"Delete every copy that is encrypted with no key? Nothing can decrypt them.", nil),
		post("Reconcile metadata", "/admin/api/reconcile",
			"Reconcile metadata against backends?", nil),
		post("Expire objects (lifecycle rules)", "/admin/api/lifecycle",
			"Apply every lifecycle rule now instead of waiting for the hourly sweep?", nil),
		post("Reconcile usage counters", "/admin/api/usage-reconcile",
			"Reconcile usage counters across all backends?", decodeOneShot[usageReconcileResult]),
		post("Flush usage counters to the database", "/admin/api/usage-flush",
			"Flush buffered usage counters to the database?", decodeOneShot[usageFlushResult]),
	}
}

// cacheActions drop cached object data. The two that take a key or a prefix
// ask for it instead of confirming: nothing is lost but a cached copy, and the
// value the operator typed is the statement of intent.
func cacheActions() []opsAction {
	return []opsAction{
		post("Flush object cache", "/admin/api/cache/flush",
			"Flush the in-memory object cache?", decodeOneShot[cacheFlushResult]),
		post("Invalidate one cached key", "/admin/api/cache/keys",
			"", decodeOneShot[cacheInvalidateKeyResult],
			deletes(),
			asks("Invalidate which key?", "bucket/path/object", func(value string) opsRequest {
				return opsRequest{path: "/admin/api/cache/keys/" + value}
			})),
		post("Invalidate a cached prefix", "/admin/api/cache/prefix",
			"", decodeOneShot[cacheFlushResult],
			deletes(),
			asks("Invalidate which prefix?", "bucket/path/", func(value string) opsRequest {
				return opsRequest{path: "/admin/api/cache/prefix", query: url.Values{"prefix": {value}}}
			})),
	}
}

// encryptionActions move stored objects between plaintext and ciphertext, or
// re-wrap the keys that seal them. Each confirmation says what the pass will
// read and rewrite, since these are metered fleet-wide operations rather than
// a setting being toggled.
// Both rewrites stream rather than answering with one summary, for the reason
// the compression pair does: they read and rewrite every object in the fleet,
// and a spinner held for the length of that is indistinguishable from a hang.
// The bounded runs are separate entries rather than a prompt on the whole-fleet
// ones, because an attached prompt refuses an empty answer.
func encryptionActions() []opsAction {
	return []opsAction{
		post("Encrypt existing objects", pathEncryptExisting,
			"Read and rewrite every plaintext copy as ciphertext?", nil),
		post("Encrypt existing objects (batch)", pathEncryptExisting,
			"Read and rewrite that many plaintext copies as ciphertext?", nil,
			asks("Encrypt how many objects?", "1000", boundedRewrite(pathEncryptExisting))),
		post("Decrypt existing objects", pathDecryptExisting,
			"Read and rewrite every encrypted copy as plaintext?", nil),
		post("Decrypt existing objects (batch)", pathDecryptExisting,
			"Read and rewrite that many encrypted copies as plaintext?", nil,
			asks("Decrypt how many objects?", "1000", boundedRewrite(pathDecryptExisting))),
		post("Rotate encryption key", "/admin/api/rotate-encryption-key",
			"Re-wrap every object key sealed with that key id?", decodeOneShot[rotateKeyResult],
			asks("Rotate away from which key id?", "old key id", func(value string) opsRequest {
				body, _ := json.Marshal(adminapi.RotateEncryptionKeyRequest{OldKeyID: value})
				return opsRequest{path: "/admin/api/rotate-encryption-key", body: body}
			})),
	}
}

// compressionActions move stored objects between verbatim and encoded. Both are
// offered whatever the write path is configured to do: converting a fleet before
// switching writes over is a legitimate order to do it in, and unwinding one is
// something an operator reaches for after turning the feature off.
//
// Both stream their progress rather than answering with one summary: they read
// and rewrite every object in the fleet, so a caller watching one needs to see
// it move rather than wait on a spinner that is indistinguishable from a hang.
// The bounded runs are separate entries rather than a prompt on the whole-fleet
// ones, because an attached prompt refuses an empty answer: it would make
// converting a whole fleet impossible to ask for, which is the common case.
func compressionActions() []opsAction {
	return []opsAction{
		post("Compress existing objects", pathCompressExisting,
			"Read and rewrite every uncompressed copy as chunked zstd?", nil),
		post("Compress existing objects (batch)", pathCompressExisting,
			"Read and rewrite that many uncompressed copies as chunked zstd?", nil,
			asks("Compress how many objects?", "1000", boundedRewrite(pathCompressExisting))),
		post("Decompress existing objects", pathDecompressExisting,
			"Read and rewrite every compressed copy back to its stored bytes?", nil),
		post("Decompress existing objects (batch)", pathDecompressExisting,
			"Read and rewrite that many compressed copies back to their stored bytes?", nil,
			asks("Decompress how many objects?", "1000", boundedRewrite(pathDecompressExisting))),
	}
}

// boundedRewrite turns the typed count into a capped request against path.
//
// Nothing is carried between runs: a rewritten copy leaves the listing that
// selected it, and one declined on ratio is recorded so it leaves too, so
// running the entry again converts the next batch rather than the last one.
func boundedRewrite(path string) func(string) opsRequest {
	return func(value string) opsRequest {
		return opsRequest{path: path, query: url.Values{"max": {value}}}
	}
}

// opsView holds the state of the ops pane's menu.
// actions is the menu this pane is showing, and backend the one every request
// in it names. The ops section fills them with the fleet-wide list and no
// backend; the backends pane fills them with one backend's list and its name,
// so the same menu, confirm and run path serves both.
//
// A named backend is also what says where esc goes: only the backends pane
// opens a scoped menu, so the pane it came from is derivable rather than
// carried, and a zero value cannot point somewhere wrong.
type opsView struct {
	cursor  int // highlighted menu row
	actions []opsAction
	backend string
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// handleOpsKey drives the menu. A run started from it is shown and driven by
// the shared run (run.go).
func (m *model) handleOpsKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "left", "h":
		return m.opsBack()
	case "up", "k":
		if m.ops.cursor > 0 {
			m.ops.cursor--
		}
		return m, nil
	case "down", "j":
		if m.ops.cursor < len(m.ops.actions)-1 {
			m.ops.cursor++
		}
		return m, nil
	case "enter", "right", "l":
		if m.ops.cursor >= len(m.ops.actions) {
			return m, nil
		}
		a := &m.ops.actions[m.ops.cursor]
		backend := m.ops.backend
		if a.ask.question != "" {
			if m.runBusy() {
				return m, nil
			}
			return m.askFor(a.ask.question, a.ask.placeholder, func(value string) adminAction {
				return m.runAction(a, scopeToBackend(a.resolve(value), backend))
			})
		}
		return m.startRun(a, scopeToBackend(opsRequest{path: a.path}, backend))
	}
	return m, nil
}

// scopeToBackend names the backend a request runs against, leaving a fleet-wide
// one untouched. Applied once here rather than in each entry, so an action added
// to either menu is scoped by the menu it was opened from.
func scopeToBackend(req opsRequest, backend string) opsRequest {
	if backend == "" {
		return req
	}
	if req.query == nil {
		req.query = url.Values{}
	}
	req.query.Set(queryBackend, backend)
	return req
}

// opsBack leaves the menu for whichever section opened it: the backends pane
// when the menu names a backend, the nav otherwise.
func (m *model) opsBack() (tea.Model, tea.Cmd) {
	if m.ops.backend != "" {
		return m.selectSection(sectionBackends)
	}
	return m.navBack()
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// opsPaneView composes the pane's full-screen layout.
func (m *model) opsPaneView() string {
	return m.frame(m.opsHeaderView(), m.hintFooter(), textPane(m.opsMenuView()))
}

// opsHeaderView renders the title bar. The scope is named, so a menu opened
// from a backend row cannot be read as the fleet-wide one.
func (m *model) opsHeaderView() string {
	scope := "ops"
	if m.ops.backend != "" {
		scope = "ops   " + m.ops.backend
	}
	return m.contentTitleStyle().Width(m.contentWidth()).Render(scope + "   select an action")
}

// opsMenuView renders the action list with the cursor marker.
func (m *model) opsMenuView() string {
	var b strings.Builder
	for i, a := range m.ops.actions {
		marker, style := "  ", navItemStyle
		if i == m.ops.cursor {
			marker, style = "> ", navActiveStyle
		}
		// A trailing ellipsis is the usual signal that a menu entry asks for
		// something before it acts, rather than acting on the spot.
		label := a.label
		if a.ask.question != "" {
			label += "..."
		}
		b.WriteString(style.Render(marker + label))
		b.WriteString("\n")
	}
	return b.String()
}
