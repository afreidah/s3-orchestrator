// -------------------------------------------------------------------------------
// TUI - Cache View Tests
//
// Author: Alex Freidah
//
// Deterministic tests for the object cache pane: the summary's capacity and
// hit-rate lines, the no-lookups-yet case, body rendering per state, key
// handling, and the split between a disabled cache and a real fetch failure.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

func TestCacheStats_CapacityAndHitRate(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.cache.snap = &adminapi.CacheStatsResponse{
		Entries: 12, SizeBytes: 2048, MaxBytes: 4096, Hits: 30, Misses: 10,
	}
	got := m.cacheStats()
	// 2048/4096 = 50% full; 30 of 40 lookups hit = 75%.
	for _, want := range []string{"entries", "12", "2.0 KiB", "4.0 KiB", "50%", "75%", "30 hits / 10 misses"} {
		if !strings.Contains(got, want) {
			t.Errorf("stats missing %q: %q", want, got)
		}
	}
}

// TestCacheStats_NoLookups covers a cache that has never been read: a 0% hit
// rate would read as a broken cache, when in fact nothing has asked it for
// anything yet.
func TestCacheStats_NoLookups(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.cache.snap = &adminapi.CacheStatsResponse{Entries: 0, MaxBytes: 4096}
	got := m.cacheStats()
	if !strings.Contains(got, "no lookups yet") {
		t.Errorf("stats = %q, want the no-lookups notice", got)
	}
	for line := range strings.SplitSeq(got, "\n") {
		if strings.HasPrefix(line, "hit rate") && strings.Contains(line, "%") {
			t.Errorf("hit-rate line reports a percentage with no lookups: %q", line)
		}
	}
}

// TestCacheStats_UnlimitedSize covers a snapshot without a max: the size line
// must not divide by zero or claim a percentage it cannot compute.
func TestCacheStats_UnlimitedSize(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.cache.snap = &adminapi.CacheStatsResponse{Entries: 1, SizeBytes: 1024}
	if got := m.cacheStats(); !strings.Contains(got, "1.0 KiB") || strings.Contains(got, "%") {
		t.Errorf("stats = %q, want a bare size with no percentage", got)
	}
}

func TestHitRateStyle_Thresholds(t *testing.T) {
	t.Parallel()
	// Inverted against usageStyle: a high hit rate is the healthy end.
	if hitRateStyle(90).GetForeground() != hitRateStyle(60).GetForeground() {
		t.Error("60 and 90 should share the healthy style")
	}
	if hitRateStyle(30).GetForeground() == hitRateStyle(90).GetForeground() {
		t.Error("30 (warn) should differ from 90 (ok)")
	}
	if hitRateStyle(5).GetForeground() == hitRateStyle(30).GetForeground() {
		t.Error("5 (error) should differ from 30 (warn)")
	}
}

func TestApplyCacheErr_SeparatesDisabled(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})

	m.applyCacheErr(&adminclient.Error{
		Status: http.StatusServiceUnavailable,
		Body:   `{"status":"disabled","reason":"object data caching is disabled"}`,
	})
	if m.cache.err != nil || m.cache.unavailable != "object data caching is disabled" {
		t.Errorf("503: err=%v unavailable=%q", m.cache.err, m.cache.unavailable)
	}

	m.applyCacheErr(errors.New("boom"))
	if m.cache.err == nil || m.cache.unavailable != "" {
		t.Errorf("generic: err=%v unavailable=%q", m.cache.err, m.cache.unavailable)
	}
}

func TestCacheBodyView_States(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})

	m.cache = cacheView{err: errors.New("boom")}
	if got := m.cacheBodyView(); !strings.Contains(got, "boom") {
		t.Errorf("error body = %q", got)
	}
	m.cache = cacheView{unavailable: "object data caching is disabled"}
	if got := m.cacheBodyView(); !strings.Contains(got, "disabled") {
		t.Errorf("disabled body = %q", got)
	}
	m.cache = cacheView{loading: true}
	if got := m.cacheBodyView(); !strings.Contains(got, "loading") {
		t.Errorf("loading body = %q", got)
	}
	m.cache = cacheView{}
	if got := m.cacheBodyView(); !strings.Contains(got, "no cache data") {
		t.Errorf("empty body = %q", got)
	}
}

func TestApplyCache_ClearsPriorUnavailable(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.cache = cacheView{loading: true, unavailable: "disabled", err: errors.New("old")}
	m.applyCache(&adminapi.CacheStatsResponse{Entries: 3})
	if m.cache.loading || m.cache.unavailable != "" || m.cache.err != nil || m.cache.snap == nil {
		t.Errorf("state = %+v", m.cache)
	}
}

func TestHandleCacheKey_BackAndReload(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionCache

	m.handleCacheKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.navFocus || m.navCursor != int(sectionCache) {
		t.Errorf("after esc: navFocus=%v cursor=%d", m.navFocus, m.navCursor)
	}

	m.navFocus = false
	_, cmd := m.handleCacheKey(key("r"))
	if cmd == nil {
		t.Fatal("reload returned no command")
	}
	if _, ok := cmd().(cacheLoadedMsg); !ok {
		t.Errorf("reload result = %#v, want cacheLoadedMsg", cmd())
	}
}

// TestHandleCacheKey_ReloadKeepsSnapshot asserts a refresh over an existing
// snapshot does not flip the pane back to a spinner, which would flash the
// numbers off screen on every reload.
func TestHandleCacheKey_ReloadKeepsSnapshot(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionCache
	m.cache.snap = &adminapi.CacheStatsResponse{Entries: 1}
	m.handleCacheKey(key("r"))
	if m.cache.loading {
		t.Error("reload over an existing snapshot set loading")
	}
}

func TestLoadCache_Error(t *testing.T) {
	t.Parallel()
	cmd := initialModel(errLister{}).loadCache()
	if _, ok := cmd().(cacheErrMsg); !ok {
		t.Errorf("cmd result = %#v, want cacheErrMsg", cmd())
	}
}
