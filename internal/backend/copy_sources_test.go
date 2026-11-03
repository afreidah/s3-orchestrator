// -------------------------------------------------------------------------------
// Backend - Copy From Any Source Tests
//
// Author: Alex Freidah
//
// Pin the source walk: the first source that reads wins, a failed read moves
// on and is reported, a write failure ends the walk, unknown backends are
// skipped, and a walk with nothing readable says so.
// -------------------------------------------------------------------------------

package backend_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// scriptedRunner answers StreamCopy per source backend from errs, recording
// each request it was sent. Backends absent from known do not resolve.
type scriptedRunner struct {
	known map[string]bool
	errs  map[string]error
	sent  []*backend.CopyRequest
}

func (r *scriptedRunner) GetBackend(name string) (backend.ObjectBackend, error) {
	if !r.known[name] {
		return nil, fmt.Errorf("backend %s not found", name)
	}
	return backendtest.NewInMemory(), nil
}

func (r *scriptedRunner) StreamCopy(_ context.Context, req *backend.CopyRequest) (string, error) {
	r.sent = append(r.sent, req)
	if err := r.errs[req.Src.Name]; err != nil {
		return "", err
	}
	return "etag-" + req.Src.Name, nil
}

// sources is one copy of the object on each named backend, each recording a
// distinct size.
func sources(names ...string) []core.ObjectLocation {
	locs := make([]core.ObjectLocation, len(names))
	for i, name := range names {
		locs[i] = core.ObjectLocation{BackendName: name, StorageKey: "k!" + name, SizeBytes: int64(i + 1)}
	}
	return locs
}

func TestCopyFromAny_FallsThroughReadFailures(t *testing.T) {
	t.Parallel()
	readErr := &backend.CopyError{Phase: backend.CopyPhaseRead, Err: errors.New("gone")}
	rt := &scriptedRunner{
		known: map[string]bool{"b1": true, "b2": true, "b3": true},
		errs:  map[string]error{"b2": readErr},
	}
	var failed []string
	src, etag, err := backend.CopyFromAny(t.Context(), rt, &backend.SourcedCopy{
		Key:     "k",
		Sources: sources("ghost", "b2", "b3"),
		Dst:     backend.Endpoint{Name: "dst"},
		DstKey:  "k!new",
		OnReadFail: func(loc *core.ObjectLocation, _ error) {
			failed = append(failed, loc.BackendName)
		},
	})
	if err != nil {
		t.Fatalf("CopyFromAny: %v", err)
	}
	if src.BackendName != "b3" || etag != "etag-b3" {
		t.Errorf("source = %s, etag = %q, want b3 and its etag", src.BackendName, etag)
	}
	if len(failed) != 1 || failed[0] != "b2" {
		t.Errorf("read failures reported = %v, want [b2]", failed)
	}
	last := rt.sent[len(rt.sent)-1]
	if last.SrcKey != "k!b3" || last.DstKey != "k!new" || last.SrcSize != 3 {
		t.Errorf("request = %s -> %s size %d, want k!b3 -> k!new size 3", last.SrcKey, last.DstKey, last.SrcSize)
	}
}

func TestCopyFromAny_WriteFailureEndsTheWalk(t *testing.T) {
	t.Parallel()
	writeErr := &backend.CopyError{Phase: backend.CopyPhaseWrite, Err: errors.New("full")}
	rt := &scriptedRunner{
		known: map[string]bool{"b1": true, "b2": true},
		errs:  map[string]error{"b1": writeErr},
	}
	_, _, err := backend.CopyFromAny(t.Context(), rt, &backend.SourcedCopy{Key: "k", Sources: sources("b1", "b2")})
	if !errors.Is(err, writeErr) {
		t.Fatalf("err = %v, want the write failure", err)
	}
	if len(rt.sent) != 1 {
		t.Errorf("copies sent = %d, want 1", len(rt.sent))
	}
}

func TestCopyFromAny_NothingReadable(t *testing.T) {
	t.Parallel()
	readErr := errors.New("gone")
	rt := &scriptedRunner{known: map[string]bool{"b1": true}, errs: map[string]error{"b1": readErr}}

	_, _, err := backend.CopyFromAny(t.Context(), rt, &backend.SourcedCopy{Key: "k", Sources: sources("b1")})
	if !errors.Is(err, backend.ErrNoSourceRead) || !errors.Is(err, readErr) {
		t.Errorf("err = %v, want ErrNoSourceRead wrapping the last read failure", err)
	}

	_, _, err = backend.CopyFromAny(t.Context(), rt, &backend.SourcedCopy{Key: "k", Sources: sources("ghost")})
	if !errors.Is(err, backend.ErrNoSourceRead) {
		t.Errorf("err = %v, want ErrNoSourceRead when no source resolves", err)
	}
}

func TestCopyFromAny_StopsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rt := &scriptedRunner{known: map[string]bool{"b1": true}}
	if _, _, err := backend.CopyFromAny(ctx, rt, &backend.SourcedCopy{Key: "k", Sources: sources("b1")}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(rt.sent) != 0 {
		t.Errorf("copies sent = %d after cancel, want 0", len(rt.sent))
	}
}
