// -------------------------------------------------------------------------------
// ListObjects Benchmarks - Delimiter-Collapse Pagination Cost
//
// Author: Alex Freidah
//
// Measures the CPU cost of Manager.ListObjects on the delimiter path, where
// many raw keys collapse into a handful of CommonPrefixes. Because the page
// loop counts emitted entries rather than scanned rows, a delimiter list of a
// wide prefix scans up to ListObjectsMaxPages*maxKeys rows to return a few
// prefixes. The delimiter variants report rows/op and pages/op so the
// amplification is visible; the no-delimiter control pins the 1:1 happy path
// where the loop breaks on the first page.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// quietLogs swaps the global slog default for a discard handler so the
// per-call audit line ListObjects emits does not flood benchmark output or
// charge stderr I/O against the measured loop. Returns a restore func.
func quietLogs() func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	return func() { slog.SetDefault(prev) }
}

// benchListStore is a minimal ObjectStores that serves a pre-sorted keyspace
// out of memory, paginating on startAfter exactly as the real store does. It
// tallies pages served and rows returned so benchmarks can report how much the
// proxy scanned relative to what it emitted.
type benchListStore struct {
	ObjectStores // embedded nil; only ListObjects is implemented

	keys  []string // pre-sorted ascending, all under the bench prefix
	pages int      // ListObjects calls served since the last reset
	rows  int      // ObjectLocations returned since the last reset
}

// ListObjects returns the keys after startAfter, capped at maxKeys, flagging
// truncation when more remain. Prefix filtering is unnecessary because the
// seeded keyspace lives entirely under the bench prefix.
func (s *benchListStore) ListObjects(_ context.Context, _, startAfter string, maxKeys int) (*core.ListObjectsResult, error) {
	lo := 0
	if startAfter != "" {
		lo = sort.Search(len(s.keys), func(i int) bool { return s.keys[i] > startAfter })
	}
	hi := lo + maxKeys
	truncated := hi < len(s.keys)
	if !truncated {
		hi = len(s.keys)
	}

	page := make([]core.ObjectLocation, 0, hi-lo)
	for i := lo; i < hi; i++ {
		page = append(page, core.ObjectLocation{ObjectKey: s.keys[i]})
	}

	s.pages++
	s.rows += len(page)
	return &core.ListObjectsResult{Objects: page, IsTruncated: truncated}, nil
}

// benchRuntime is a minimal ObjectRuntime: ListObjects only reaches the runtime
// through Acct().Operation, so every other method stays unimplemented.
type benchRuntime struct {
	ObjectRuntime // embedded nil; only Acct is implemented

	acct *accounting.Recorder
}

// Acct returns the no-op recorder the benchmark wires in.
func (r benchRuntime) Acct() *accounting.Recorder { return r.acct }

// benchListPrefix is the top-level prefix every seeded key shares.
const benchListPrefix = "logs/"

// benchListDirs is the number of second-level directories the keyspace spreads
// across, so a delimiter list emits this many CommonPrefixes regardless of how
// many underlying keys exist.
const benchListDirs = 8

// seedListKeys builds n sorted keys spread evenly across benchListDirs
// second-level directories under benchListPrefix. With delimiter "/" they
// collapse to benchListDirs CommonPrefixes.
func seedListKeys(n int) []string {
	keys := make([]string, 0, n)
	perDir := n / benchListDirs
	for d := range benchListDirs {
		for i := range perDir {
			keys = append(keys, fmt.Sprintf("%sdir%02d/key%08d.txt", benchListPrefix, d, i))
		}
	}
	sort.Strings(keys)
	return keys
}

// newBenchListManager assembles a Manager wired only with what ListObjects
// touches: an in-memory store and a no-op accounting recorder.
func newBenchListManager(store ObjectStores) *Manager {
	rec := accounting.New(nil, func(_, _ string, _ time.Time, _ error) {})
	return &Manager{
		stores: store,
		core:   benchRuntime{acct: rec},
		log:    slog.Default(),
	}
}

// BenchmarkListObjects_DelimiterCollapse measures the delimiter path as the
// underlying keyspace grows while the emitted CommonPrefix count stays fixed.
// CPU should scale with keyspace size here - that is the pathology. rows/op and
// pages/op are reported so the scan-vs-emit amplification is explicit.
func BenchmarkListObjects_DelimiterCollapse(b *testing.B) {
	defer quietLogs()()
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("keys=%d", n), func(b *testing.B) {
			store := &benchListStore{keys: seedListKeys(n)}
			m := newBenchListManager(store)
			ctx := context.Background()

			var lastKeyCount int
			store.pages, store.rows = 0, 0
			iters := 0
			b.ResetTimer()
			for b.Loop() {
				res, err := m.ListObjects(ctx, benchListPrefix, "/", "", 1000)
				if err != nil {
					b.Fatalf("ListObjects: %v", err)
				}
				lastKeyCount = res.KeyCount
				iters++
			}
			b.StopTimer()

			b.ReportMetric(float64(store.rows)/float64(iters), "rows/op")
			b.ReportMetric(float64(store.pages)/float64(iters), "pages/op")
			b.ReportMetric(float64(lastKeyCount), "emitted")
		})
	}
}

// BenchmarkListObjects_NoDelimiterControl is the control: the same keyspace
// sizes with no delimiter, where every row maps 1:1 to an emitted object so the
// loop fills maxKeys on the first page and breaks. CPU should stay flat across
// sizes, isolating the delimiter collapse as the cost driver.
func BenchmarkListObjects_NoDelimiterControl(b *testing.B) {
	defer quietLogs()()
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("keys=%d", n), func(b *testing.B) {
			store := &benchListStore{keys: seedListKeys(n)}
			m := newBenchListManager(store)
			ctx := context.Background()

			store.pages, store.rows = 0, 0
			iters := 0
			b.ResetTimer()
			for b.Loop() {
				if _, err := m.ListObjects(ctx, benchListPrefix, "", "", 1000); err != nil {
					b.Fatalf("ListObjects: %v", err)
				}
				iters++
			}
			b.StopTimer()

			b.ReportMetric(float64(store.rows)/float64(iters), "rows/op")
			b.ReportMetric(float64(store.pages)/float64(iters), "pages/op")
		})
	}
}
