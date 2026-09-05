// -------------------------------------------------------------------------------
// Lifecycle Expiration - End-to-End Integration Tests
//
// Author: Alex Freidah
//
// Drives the real expiry manager against real Postgres, so a rule's filter is
// carried from config through to the SQL that selects what dies. The unit tests
// stub the store and the store tests skip the rule evaluation, which leaves the
// join between them - the part that decides whether the wrong objects are
// deleted - covered by neither.
//
// Deadlines are deliberately short: a negative expiration puts the cutoff in
// the future so every seeded object is immediately eligible, which is what lets
// a test observe a full sweep without waiting a day for one.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/expiry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// recordingDeleter stands in for the object manager, capturing which keys a
// sweep decided to delete without removing anything. What is under test is the
// selection, and a real delete would take the rows out from under the
// assertions.
type recordingDeleter struct {
	mu   sync.Mutex
	keys []string
}

func (d *recordingDeleter) DeleteObject(_ context.Context, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.keys = append(d.keys, key)
	return nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// deleted returns the captured keys, sorted.
func (d *recordingDeleter) deleted() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := slices.Clone(d.keys)
	sort.Strings(out)
	return out
}

// sweep runs one set of rules against the store and reports the keys selected.
//
// A single batch is forced so a rule stops after one pass: applyRule keeps
// querying until a short batch arrives, and nothing here deletes the rows it
// just matched.
func sweep(t *testing.T, s *Store, rules []config.LifecycleRule) []string {
	t.Helper()
	deleter := &recordingDeleter{}
	m := expiry.New(s, deleter, nil)
	m.SetConfig(&config.LifecycleConfig{BatchSize: 100})
	m.ProcessRules(context.Background(), rules, nil)
	return deleter.deleted()
}

// seedTagged records one object per entry, tagged as described.
func seedTagged(t *testing.T, s *Store, objects map[string][]core.Tag) {
	t.Helper()
	for key, tags := range objects {
		if _, _, err := s.RecordObject(context.Background(), &core.RecordObjectRequest{
			Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100, Tags: tags,
		}); err != nil {
			t.Fatalf("RecordObject %s: %v", key, err)
		}
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestLifecycleInt_TagFilterEndToEnd proves a tagged rule expires only the
// objects carrying its tags, and leaves everything else alone.
func TestLifecycleInt_TagFilterEndToEnd(t *testing.T) {
	s := adapterPgStore(t)
	prefix := t.Name() + "/"

	seedTagged(t, s, map[string][]core.Tag{
		prefix + "staging-infra": {{Key: "env", Value: "staging"}, {Key: "team", Value: "infra"}},
		prefix + "staging-only":  {{Key: "env", Value: "staging"}},
		prefix + "prod":          {{Key: "env", Value: "prod"}},
		prefix + "untagged":      nil,
	})

	t.Run("one tag expires only what carries it", func(t *testing.T) {
		got := sweep(t, s, []config.LifecycleRule{{
			Prefix:         prefix,
			Tags:           map[string]string{"env": "staging"},
			ExpirationDays: -1,
		}})
		want := []string{prefix + "staging-infra", prefix + "staging-only"}
		if !slices.Equal(got, want) {
			t.Errorf("deleted %v, want %v", got, want)
		}
	})

	t.Run("two tags expire only their intersection", func(t *testing.T) {
		got := sweep(t, s, []config.LifecycleRule{{
			Prefix:         prefix,
			Tags:           map[string]string{"env": "staging", "team": "infra"},
			ExpirationDays: -1,
		}})
		want := []string{prefix + "staging-infra"}
		if !slices.Equal(got, want) {
			t.Errorf("deleted %v, want %v", got, want)
		}
	})

	t.Run("a rule matching nothing deletes nothing", func(t *testing.T) {
		got := sweep(t, s, []config.LifecycleRule{{
			Prefix:         prefix,
			Tags:           map[string]string{"env": "absent"},
			ExpirationDays: -1,
		}})
		if len(got) != 0 {
			t.Errorf("deleted %v, want nothing", got)
		}
	})

	t.Run("a tags-only rule reaches across prefixes", func(t *testing.T) {
		got := sweep(t, s, []config.LifecycleRule{{
			Tags:           map[string]string{"team": "infra"},
			ExpirationDays: -1,
		}})
		if !slices.Contains(got, prefix+"staging-infra") {
			t.Errorf("deleted %v, want it to include the infra-tagged object", got)
		}
	})
}

// TestLifecycleInt_UnexpiredObjectsSurvive proves the cutoff is still honoured
// under a tag filter: a positive expiration leaves freshly written objects in
// place, so a tagged rule cannot delete an object that is simply too young.
func TestLifecycleInt_UnexpiredObjectsSurvive(t *testing.T) {
	s := adapterPgStore(t)
	prefix := t.Name() + "/"

	seedTagged(t, s, map[string][]core.Tag{
		prefix + "fresh": {{Key: "env", Value: "staging"}},
	})

	got := sweep(t, s, []config.LifecycleRule{{
		Prefix:         prefix,
		Tags:           map[string]string{"env": "staging"},
		ExpirationDays: 1,
	}})
	if len(got) != 0 {
		t.Errorf("deleted %v, want nothing for an object written moments ago", got)
	}
}

// TestLifecycleInt_RulesAreIndependent proves several rules behave as an "or":
// each is evaluated against its own filter, and an object matching any one of
// them is expired.
func TestLifecycleInt_RulesAreIndependent(t *testing.T) {
	s := adapterPgStore(t)
	prefix := t.Name() + "/"

	seedTagged(t, s, map[string][]core.Tag{
		prefix + "by-tag":    {{Key: "scratch", Value: "true"}},
		prefix + "by-prefix": nil,
		prefix + "neither":   {{Key: "keep", Value: "true"}},
	})

	got := sweep(t, s, []config.LifecycleRule{
		{Prefix: prefix + "by-prefix", ExpirationDays: -1},
		{Tags: map[string]string{"scratch": "true"}, ExpirationDays: -1},
	})

	for _, want := range []string{prefix + "by-tag", prefix + "by-prefix"} {
		if !slices.Contains(got, want) {
			t.Errorf("deleted %v, want it to include %s", got, want)
		}
	}
	if slices.Contains(got, prefix+"neither") {
		t.Errorf("deleted %v, want it to leave the unmatched object alone", got)
	}
}
