// -------------------------------------------------------------------------------
// Rebalancer Planning Helpers - Placement Set and Candidate Cache
//
// Author: Alex Freidah
//
// Small helpers the rebalance planners share. placementSet answers "does this
// key already live on this backend?" in O(1) so the per-object loops avoid
// slice scans and per-object lookup-map allocations. candidateCache memoizes a
// source backend's candidate objects together with their placement set, so the
// pack planner never re-lists or re-queries the same source when pairing it
// against multiple destinations.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// placementSet maps an object key to the set of backends that already hold it.
// Built once from a copy map (key -> backend names) so planners can ask "is key
// already on backend?" without rescanning a slice or allocating a lookup map
// per object.
type placementSet map[string]map[string]struct{}

// newPlacementSet builds a placementSet from a key -> backend-names copy map.
func newPlacementSet(copyMap map[string][]string) placementSet {
	p := make(placementSet, len(copyMap))
	for key, backends := range copyMap {
		set := make(map[string]struct{}, len(backends))
		for _, b := range backends {
			set[b] = struct{}{}
		}
		p[key] = set
	}
	return p
}

// Has reports whether key already lives on backend.
func (p placementSet) Has(key, backend string) bool {
	_, ok := p[key][backend]
	return ok
}

// sourceCandidates bundles a source backend's candidate objects with their
// placement set, fetched together so a planner pays the two store queries once
// per source.
type sourceCandidates struct {
	Objects   []core.ObjectLocation
	Placement placementSet
}

// candidateCache memoizes sourceCandidates per source backend across a single
// planning run. The pack planner pairs each source against several
// destinations; without the cache it would re-list the source and re-query its
// copy map on every pairing.
type candidateCache struct {
	r         *Rebalancer
	limit     int
	byBackend map[string]sourceCandidates
}

// newCandidateCache returns a candidate cache that lists up to limit objects
// per source.
func (r *Rebalancer) newCandidateCache(limit int) *candidateCache {
	return &candidateCache{r: r, limit: limit, byBackend: make(map[string]sourceCandidates)}
}

// forBackend returns the cached candidates for src, fetching (list + copy map)
// on the first request and reusing the result thereafter.
func (c *candidateCache) forBackend(ctx context.Context, src string) (sourceCandidates, error) {
	if sc, ok := c.byBackend[src]; ok {
		return sc, nil
	}
	objects, err := c.r.d.Store.ListObjectsByBackend(ctx, src, c.limit, core.SizeCursor{})
	if err != nil {
		return sourceCandidates{}, fmt.Errorf("failed to list objects on %s: %w", src, err)
	}
	copyMap, err := c.r.fetchCopyMap(ctx, objects)
	if err != nil {
		return sourceCandidates{}, err
	}
	sc := sourceCandidates{Objects: objects, Placement: newPlacementSet(copyMap)}
	c.byBackend[src] = sc
	return sc, nil
}
