// -------------------------------------------------------------------------------
// Rebalancer Planning Helpers - Placement Set and Candidate Cache
//
// Author: Alex Freidah
//
// Small helpers the rebalance planners share. PlacementSet answers "does this
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

// PlacementSet maps an object key to the set of backends that already hold it.
// Built once from a copy map (key -> backend names) so planners can ask "is key
// already on backend?" without rescanning a slice or allocating a lookup map
// per object.
type PlacementSet map[string]map[string]struct{}

// NewPlacementSet builds a PlacementSet from a key -> backend-names copy map.
func NewPlacementSet(copyMap map[string][]string) PlacementSet {
	p := make(PlacementSet, len(copyMap))
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
func (p PlacementSet) Has(key, backend string) bool {
	_, ok := p[key][backend]
	return ok
}

// SourceCandidates bundles a source backend's candidate objects with their
// placement set, fetched together so a planner pays the two store queries once
// per source.
type SourceCandidates struct {
	Objects   []core.ObjectLocation
	Placement PlacementSet
}

// candidateCache memoizes SourceCandidates per source backend across a single
// planning run. The pack planner pairs each source against several
// destinations; without the cache it would re-list the source and re-query its
// copy map on every pairing.
type candidateCache struct {
	r         *Rebalancer
	limit     int
	byBackend map[string]SourceCandidates
}

// newCandidateCache returns a candidate cache that lists up to limit objects
// per source.
func (r *Rebalancer) newCandidateCache(limit int) *candidateCache {
	return &candidateCache{r: r, limit: limit, byBackend: make(map[string]SourceCandidates)}
}

// forBackend returns the cached candidates for src, fetching (list + copy map)
// on the first request and reusing the result thereafter.
func (c *candidateCache) forBackend(ctx context.Context, src string) (SourceCandidates, error) {
	if sc, ok := c.byBackend[src]; ok {
		return sc, nil
	}
	objects, err := c.r.store.ListObjectsByBackend(ctx, src, c.limit)
	if err != nil {
		return SourceCandidates{}, fmt.Errorf("failed to list objects on %s: %w", src, err)
	}
	copyMap, err := c.r.fetchCopyMap(ctx, objects)
	if err != nil {
		return SourceCandidates{}, err
	}
	sc := SourceCandidates{Objects: objects, Placement: NewPlacementSet(copyMap)}
	c.byBackend[src] = sc
	return sc, nil
}
