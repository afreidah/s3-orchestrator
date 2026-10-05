// -------------------------------------------------------------------------------
// TUI - Sortable Table Tests
//
// Author: Alex Freidah
//
// Covers stepping the sort through the sortable columns and back to the
// server's order, reversing it, sorting on underlying values rather than
// rendered text, keeping the selection on the same item through a re-sort or
// a refresh, and marking the sorted column's header.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	tea "github.com/charmbracelet/bubbletea"
)

// sortedBackends returns a backends list holding three backends whose sizes
// would sort wrongly as text: "900.0 MiB" sorts after "1.2 GiB" as a string.
func sortedBackends() sortTable[adminapi.BackendStatus] {
	list := newBackendsView().list
	list.setItems([]adminapi.BackendStatus{
		{Name: "b", BytesUsed: 1288490188},
		{Name: "c", BytesUsed: 943718400},
		{Name: "a", BytesUsed: 5 << 30},
	})
	return list
}

// backendOrder lists the items' names in display order.
func backendOrder(list *sortTable[adminapi.BackendStatus]) string {
	out := make([]string, len(list.items))
	for i := range list.items {
		out[i] = list.items[i].Name
	}
	return strings.Join(out, ",")
}

// TestSortTable_StepsThroughColumnsAndBack verifies "s" moves the sort from
// the server's order through each sortable column in turn, and back to the
// server's order after the last.
func TestSortTable_StepsThroughColumnsAndBack(t *testing.T) {
	t.Parallel()
	list := sortedBackends()
	if got := backendOrder(&list); got != "b,c,a" {
		t.Fatalf("initial order = %s, want the server's b,c,a", got)
	}

	list.update(key("s"))
	if list.sort.column != "BACKEND" || backendOrder(&list) != "a,b,c" {
		t.Errorf("first s: column=%q order=%s, want BACKEND a,b,c", list.sort.column, backendOrder(&list))
	}
	for range len(backendColumns) - 1 {
		list.update(key("s"))
	}
	if list.sort.column != "SAVED" {
		t.Errorf("after every column, sort = %q, want SAVED", list.sort.column)
	}
	list.update(key("s"))
	if list.sort.column != "" || backendOrder(&list) != "b,c,a" {
		t.Errorf("past the last column: column=%q order=%s, want the server's order", list.sort.column, backendOrder(&list))
	}
}

// TestSortTable_SortsOnValues verifies a size column orders by bytes, not by
// the rendered text, and that "S" reverses it.
func TestSortTable_SortsOnValues(t *testing.T) {
	t.Parallel()
	list := sortedBackends()
	list.sort = tableSort{column: "USED"}
	list.reorder("")
	if got := backendOrder(&list); got != "c,b,a" {
		t.Errorf("USED ascending = %s, want c,b,a", got)
	}
	list.update(key("S"))
	if got := backendOrder(&list); got != "a,b,c" || !list.sort.desc {
		t.Errorf("USED descending = %s desc=%v, want a,b,c", got, list.sort.desc)
	}
}

// TestSortTable_ReverseNeedsASort verifies "S" does nothing while the table
// is in the server's order, since there is no column to reverse.
func TestSortTable_ReverseNeedsASort(t *testing.T) {
	t.Parallel()
	list := sortedBackends()
	list.update(key("S"))
	if list.sort.desc || backendOrder(&list) != "b,c,a" {
		t.Errorf("S with no sort: desc=%v order=%s", list.sort.desc, backendOrder(&list))
	}
}

// TestSortTable_KeepsTheSelection verifies the cursor stays on the same item
// when the order changes and when a refresh brings new items in a new order.
func TestSortTable_KeepsTheSelection(t *testing.T) {
	t.Parallel()
	list := sortedBackends()
	list.table.SetCursor(1)
	list.update(key("s"))
	if got := list.selectedKey(); got != "c" {
		t.Errorf("after sorting, selected = %q, want c", got)
	}

	list.setItems([]adminapi.BackendStatus{{Name: "c"}, {Name: "z"}, {Name: "a"}})
	if got := list.selectedKey(); got != "c" {
		t.Errorf("after a refresh, selected = %q, want c", got)
	}
	if got := backendOrder(&list); got != "a,c,z" {
		t.Errorf("a refresh while sorted = %s, want the sort kept: a,c,z", got)
	}
}

// TestSortTable_SkipsUnsortableColumns verifies a column without a comparator
// is never chosen.
func TestSortTable_SkipsUnsortableColumns(t *testing.T) {
	t.Parallel()
	list := newSortTable(testColumns, map[string]func(a, b *adminapi.BackendStatus) int{
		"B": by(func(b *adminapi.BackendStatus) string { return b.Name }),
	}, rowsFromBackends, func(b *adminapi.BackendStatus) string { return b.Name })
	list.update(key("s"))
	if list.sort.column != "B" {
		t.Errorf("first sortable column = %q, want B", list.sort.column)
	}
	list.update(key("s"))
	if list.sort.column != "" {
		t.Errorf("after the only sortable column, sort = %q, want the server's order", list.sort.column)
	}
}

// TestSortTable_MarksTheSortedColumn verifies the sorted column's title shows
// the direction and widens to fit the marker.
func TestSortTable_MarksTheSortedColumn(t *testing.T) {
	t.Parallel()
	list := sortedBackends()
	if specs := list.specs(); specs[0].title != "BACKEND" {
		t.Errorf("unsorted title = %q, want it unmarked", specs[0].title)
	}
	list.sort = tableSort{column: "USE%"}
	spec := list.specs()[5]
	if spec.title != "USE%"+sortAscMarker || spec.min != 6+len(sortAscMarker) {
		t.Errorf("ascending spec = %+v, want a marked, widened USE%%", spec)
	}
	list.sort.desc = true
	if got := list.specs()[5].title; got != "USE%"+sortDescMarker {
		t.Errorf("descending title = %q", got)
	}
	if backendColumns[5].title != "USE%" {
		t.Error("marking a column changed the shared column specs")
	}
}

// TestSortTable_PanesSortWithS verifies every sortable pane hands "s" to its
// table rather than treating it as its own key.
func TestSortTable_PanesSortWithS(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.handleBackendsKey(key("s"))
	if m.backends.list.sort.column == "" {
		t.Error("backends: s did not sort")
	}

	m = bucketsModel(t)
	m.handleBucketsKey(key("s"))
	if m.buckets.list.sort.column == "" {
		t.Error("buckets: s did not sort")
	}

	m = cleanupModel(t)
	m.handleCleanupKey(key("s"))
	m.cleanup.tab = cleanupTabDLQ
	m.handleCleanupKey(key("s"))
	if m.cleanup.queue.sort.column == "" || m.cleanup.dlq.sort.column == "" {
		t.Error("cleanup: s did not sort the active listing")
	}

	m = initialModel(&fakeLister{})
	m.section = sectionWorkers
	m.applyWorkers(&adminapi.WorkersResponse{Workers: []adminapi.WorkerHealth{{Name: "a"}}})
	m.handleWorkersKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	if m.workers.list.sort.column == "" {
		t.Error("workers: s did not sort")
	}
}

// TestSorts_CoverTheirColumns verifies every comparator names a real column,
// so a renamed column cannot silently stop sorting.
func TestSorts_CoverTheirColumns(t *testing.T) {
	t.Parallel()
	check := func(name string, columns []columnSpec, sortable []string) {
		titles := map[string]bool{}
		for _, c := range columns {
			titles[c.title] = true
		}
		for _, s := range sortable {
			if !titles[s] {
				t.Errorf("%s: comparator for %q has no column", name, s)
			}
		}
	}
	check("backends", backendColumns, keysOf(backendSorts))
	check("buckets", bucketColumns, keysOf(bucketSorts))
	check("workers", workerColumns, keysOf(workerSorts))
	check("cleanup queue", cleanupQueueColumns, keysOf(cleanupQueueSorts))
	check("cleanup dlq", cleanupDLQColumns, keysOf(cleanupDLQSorts))
}

// keysOf lists a map's keys.
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSorts_OrderEveryColumn exercises each comparator once, so a comparator
// that reads the wrong field or panics on a zero value is caught.
func TestSorts_OrderEveryColumn(t *testing.T) {
	t.Parallel()
	b1, b2 := adminapi.BackendStatus{Name: "a"}, adminapi.BackendStatus{Name: "b", Healthy: true, BytesLimit: 1, BytesUsed: 1}
	assertOrders(t, "backends", backendSorts, b1, b2)
	e1, e2 := bucketEntry{Bucket: adminapi.Bucket{Name: "a"}}, bucketEntry{Bucket: adminapi.Bucket{Name: "b", MaxMultipartUploads: 3}, reachedBy: []string{"ci"}}
	assertOrders(t, "buckets", bucketSorts, e1, e2)
	if bucketSorts["MULTIPART"](&e1, &e2) <= 0 {
		t.Error("an unlimited multipart cap should sort above a real one")
	}
	assertOrders(t, "workers", workerSorts, adminapi.WorkerHealth{Name: "a"}, adminapi.WorkerHealth{Name: "b", ConsecutiveFailures: 1})
	assertOrders(t, "cleanup queue", cleanupQueueSorts, adminapi.CleanupQueueItem{ObjectKey: "a"}, adminapi.CleanupQueueItem{ObjectKey: "b", Attempts: 1})
	assertOrders(t, "cleanup dlq", cleanupDLQSorts, adminapi.CleanupDLQItem{ObjectKey: "a"}, adminapi.CleanupDLQItem{ObjectKey: "b", Attempts: 1})
}

// assertOrders checks every comparator treats an item as equal to itself and
// is antisymmetric across two different items.
func assertOrders[T any](t *testing.T, name string, sorts map[string]func(a, b *T) int, x, y T) {
	t.Helper()
	for title, cmp := range sorts {
		if cmp(&x, &x) != 0 {
			t.Errorf("%s %s: an item does not equal itself", name, title)
		}
		if cmp(&x, &y) != -cmp(&y, &x) {
			t.Errorf("%s %s: the order is not antisymmetric", name, title)
		}
	}
}
