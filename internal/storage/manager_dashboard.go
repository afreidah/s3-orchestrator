// -------------------------------------------------------------------------------
// Dashboard Data - Aggregated Stats for the Web UI
//
// Author: Alex Freidah
//
// Exposes a single GetDashboardData method that fetches all operational stats
// needed for the web dashboard in one call. Delegates to the underlying
// MetadataStore, benefiting from the circuit breaker when wired through
// CircuitBreakerStore.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"sort"
	"strings"
)

// DashboardData holds a snapshot of all operational data for the dashboard.
type DashboardData struct {
	BackendOrder          []string
	QuotaStats            map[string]QuotaStat
	ObjectCounts          map[string]int64
	ActiveMultipartCounts map[string]int64
	UsageStats            map[string]UsageStat
	UsageLimits           map[string]UsageLimits
	UsagePeriod           string
	ObjectTree            []*ObjectNode
}

// ObjectNode represents a directory or file in the object tree.
type ObjectNode struct {
	Name      string        // directory or file name
	IsDir     bool          // true for directories
	Children  []*ObjectNode // subdirectories and files (sorted: dirs first, then files)
	Backend   string        // only for files
	SizeBytes int64         // total size (sum of descendants for dirs, own size for files)
	Count     int           // total file count (sum of descendants for dirs, 1 for files)
	CreatedAt string        // only for files, formatted as "2006-01-02 15:04"
}

// buildObjectTree groups flat objects into a nested tree by path segments.
func buildObjectTree(objects []ObjectLocation) []*ObjectNode {
	root := &ObjectNode{IsDir: true}

	for _, obj := range objects {
		parts := strings.Split(obj.ObjectKey, "/")
		node := root
		for i, part := range parts {
			isLeaf := i == len(parts)-1
			if isLeaf {
				node.Children = append(node.Children, &ObjectNode{
					Name:      part,
					Backend:   obj.BackendName,
					SizeBytes: obj.SizeBytes,
					Count:     1,
					CreatedAt: obj.CreatedAt.Format("2006-01-02 15:04"),
				})
			} else {
				child := findChild(node, part)
				if child == nil {
					child = &ObjectNode{Name: part, IsDir: true}
					node.Children = append(node.Children, child)
				}
				node = child
			}
		}
	}

	// Recursively sort and compute rollup stats.
	computeStats(root)
	return root.Children
}

func findChild(node *ObjectNode, name string) *ObjectNode {
	for _, c := range node.Children {
		if c.IsDir && c.Name == name {
			return c
		}
	}
	return nil
}

func computeStats(node *ObjectNode) {
	if !node.IsDir {
		return
	}
	node.SizeBytes = 0
	node.Count = 0
	for _, c := range node.Children {
		computeStats(c)
		node.SizeBytes += c.SizeBytes
		node.Count += c.Count
	}
	// Sort: directories first (alphabetical), then files (alphabetical).
	sort.Slice(node.Children, func(i, j int) bool {
		a, b := node.Children[i], node.Children[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return a.Name < b.Name
	})
}

// GetDashboardData fetches all stats needed for the web UI in one call.
func (m *BackendManager) GetDashboardData(ctx context.Context) (*DashboardData, error) {
	data := &DashboardData{
		BackendOrder: m.order,
		UsageLimits:  m.usageLimits,
		UsagePeriod:  currentPeriod(),
	}

	var err error

	data.QuotaStats, err = m.store.GetQuotaStats(ctx)
	if err != nil {
		return nil, err
	}

	data.ObjectCounts, err = m.store.GetObjectCounts(ctx)
	if err != nil {
		return nil, err
	}

	data.ActiveMultipartCounts, err = m.store.GetActiveMultipartCounts(ctx)
	if err != nil {
		return nil, err
	}

	data.UsageStats, err = m.store.GetUsageForPeriod(ctx, data.UsagePeriod)
	if err != nil {
		return nil, err
	}

	// Fetch all objects (up to 1000) and build the tree for the file browser.
	listResult, err := m.store.ListObjects(ctx, "", "", 1000)
	if err != nil {
		return nil, err
	}
	data.ObjectTree = buildObjectTree(listResult.Objects)

	return data, nil
}
