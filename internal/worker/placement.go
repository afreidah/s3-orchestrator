// -------------------------------------------------------------------------------
// Rebalancer Planning Helpers - Placement Set and Candidate Cache
//
// Author: Alex Freidah
//
// Small helpers the rebalance planners share. placementSet records which
// backends hold which keys, so one planning run judges every move against the
// same picture. candidateCache memoizes a source backend's candidate objects,
// so the pack planner never re-lists or re-queries a source when pairing it
// against multiple destinations.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// copyAt names one copy: a key on a backend.
type copyAt struct {
	key     string
	backend string
}

// placementSet is a set of copies, so a planner can ask whether a key lives on
// a backend without rescanning a slice.
type placementSet map[copyAt]struct{}

// add records one copy.
func (p placementSet) add(key, backend string) {
	p[copyAt{key, backend}] = struct{}{}
}

// addAll records every copy in a key -> backend-names copy map, keeping what
// the set already holds.
func (p placementSet) addAll(copyMap map[string][]string) {
	for key, backends := range copyMap {
		for _, b := range backends {
			p.add(key, b)
		}
	}
}

// Has reports whether key lives on backend.
func (p placementSet) Has(key, backend string) bool {
	_, ok := p[copyAt{key, backend}]
	return ok
}

// candidateCache memoizes a source's candidate objects across a single planning
// run, and records where their keys live in holders as it fetches them. The
// pack planner pairs each source against several destinations; without the
// cache it would re-list the source and re-query its copy map on every pairing.
type candidateCache struct {
	r         *Rebalancer
	limit     int
	holders   placementSet
	byBackend map[string][]core.ObjectLocation
}

// newCandidateCache returns a candidate cache that lists up to limit objects
// per source and records their copies in holders.
func (r *Rebalancer) newCandidateCache(limit int, holders placementSet) *candidateCache {
	return &candidateCache{r: r, limit: limit, holders: holders, byBackend: make(map[string][]core.ObjectLocation)}
}

// forBackend returns the cached candidates for src, fetching them on the first
// request. A failed copy lookup fails planning rather than leaving holders
// empty, which would plan transfers to destinations that already hold a copy.
func (c *candidateCache) forBackend(ctx context.Context, src string) ([]core.ObjectLocation, error) {
	if objects, ok := c.byBackend[src]; ok {
		return objects, nil
	}
	objects, err := c.r.d.Store.ListObjectsByBackend(ctx, src, c.limit, core.SizeCursor{})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects on %s: %w", src, err)
	}
	copyMap, err := c.r.d.Store.GetObjectBackendsForKeys(ctx, core.KeysOf(objects))
	if err != nil {
		return nil, fmt.Errorf("fetch copy map for rebalance planning: %w", err)
	}
	c.holders.addAll(copyMap)
	c.byBackend[src] = objects
	return objects, nil
}
