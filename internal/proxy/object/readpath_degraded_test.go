// -------------------------------------------------------------------------------
// Degraded Read Path Tests
//
// Author: Alex Freidah
//
// Covers which bytes a GET or HEAD reads while the database is down and no row
// says where an object's bytes are: the newest of the bare key and its
// "!<id>" copies, never another object whose name starts with the same
// characters.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// copyIDOld and copyIDNew are well-formed per-write ids: 32 lowercase hex
// characters each.
const (
	copyIDOld = "00000000000000000000000000000001"
	copyIDNew = "00000000000000000000000000000002"
)

// TestGetObject_DBUnavailable_ReadsTheNewestCopy verifies a degraded GET picks
// the newest of the bare key and its copies, and skips keys that only share
// the object's name as a prefix.
func TestGetObject_DBUnavailable_ReadsTheNewestCopy(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		objects map[string]time.Time
		want    string
	}{
		{"bare key only", map[string]time.Time{"key": base}, "key"},
		{"copy only", map[string]time.Time{"key!" + copyIDOld: base}, "key!" + copyIDOld},
		{"copy newer than the bare key", map[string]time.Time{
			"key": base, "key!" + copyIDOld: base.Add(time.Hour),
		}, "key!" + copyIDOld},
		{"newest of two copies", map[string]time.Time{
			"key!" + copyIDOld: base, "key!" + copyIDNew: base.Add(time.Hour),
		}, "key!" + copyIDNew},
		{"another object named key!backup is skipped", map[string]time.Time{
			"key!" + copyIDOld: base, "key!backup": base.Add(time.Hour),
		}, "key!" + copyIDOld},
		{"another object named key.bak is skipped", map[string]time.Time{
			"key!" + copyIDOld: base, "key.bak": base.Add(time.Hour),
		}, "key!" + copyIDOld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			be := backendtest.NewInMemory()
			for k, at := range tc.objects {
				be.Objects[k] = backendtest.Object{Data: []byte(k), LastModified: at}
			}
			mgr := newFleet(t, locationsStore(t, nil, core.ErrDBUnavailable), map[string]backend.ObjectBackend{"b1": be}, nil)

			result, err := mgr.GetObject(context.Background(), "key", "")
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}
			defer func() { _ = result.Body.Close() }()
			got, _ := io.ReadAll(result.Body)
			if string(got) != tc.want {
				t.Errorf("served the bytes stored at %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHeadObject_DBUnavailable_ReadsTheNewestCopy verifies a degraded HEAD
// reports the newest copy, not the bare key.
func TestHeadObject_DBUnavailable_ReadsTheNewestCopy(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	be := backendtest.NewInMemory()
	be.Objects["key"] = backendtest.Object{Data: []byte("old"), LastModified: base}
	be.Objects["key!"+copyIDNew] = backendtest.Object{Data: []byte("newer copy"), LastModified: base.Add(time.Hour)}
	mgr := newFleet(t, locationsStore(t, nil, core.ErrDBUnavailable), map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if result.Size != int64(len("newer copy")) {
		t.Errorf("size = %d, want the newer copy's %d", result.Size, len("newer copy"))
	}
}

// compressedFleet stores data compressed by a real codec at key on one backend
// and returns a manager configured with that codec whose database is down.
func compressedFleet(t *testing.T, key string, data []byte) *fleet {
	t.Helper()
	codec, err := compression.NewCodec(compression.DefaultLevel, 64*1024)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	var stored bytes.Buffer
	if _, err := codec.Compress(&stored, bytes.NewReader(data)); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	be := backendtest.NewInMemory()
	be.Objects[key] = backendtest.Object{Data: stored.Bytes()}
	return newFleet(t, locationsStore(t, nil, core.ErrDBUnavailable), map[string]backend.ObjectBackend{"b1": be}, &fleetOpts{Codec: codec})
}

// TestGetObject_DBUnavailable_DecodesACompressedCopy verifies a degraded GET of
// a compressed copy serves the client's bytes, not the stored frames, for a
// whole read and for a range.
func TestGetObject_DBUnavailable_DecodesACompressedCopy(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("compressible "), 4096)
	mgr := compressedFleet(t, "key!"+copyIDNew, data)

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	got, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if !bytes.Equal(got, data) {
		t.Errorf("served %d bytes that do not match the %d bytes written", len(got), len(data))
	}

	ranged, err := mgr.GetObject(context.Background(), "key", "bytes=13-25")
	if err != nil {
		t.Fatalf("ranged GetObject: %v", err)
	}
	got, _ = io.ReadAll(ranged.Body)
	_ = ranged.Body.Close()
	if !bytes.Equal(got, data[13:26]) {
		t.Errorf("range served %q, want %q", got, data[13:26])
	}
}

// TestHeadObject_DBUnavailable_ReportsTheLogicalSize verifies a degraded HEAD
// of a compressed copy reports the size the client wrote, not the stored size.
func TestHeadObject_DBUnavailable_ReportsTheLogicalSize(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("compressible "), 4096)
	mgr := compressedFleet(t, "key", data)

	result, err := mgr.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if result.Size != int64(len(data)) {
		t.Errorf("size = %d, want the logical %d", result.Size, len(data))
	}
}

// TestGetObject_DBUnavailable_ServesAPlainZstdFileAsIs verifies a .zst file a
// client uploaded, which has no seek table, is served as the bytes it is.
func TestGetObject_DBUnavailable_ServesAPlainZstdFileAsIs(t *testing.T) {
	t.Parallel()
	codec, err := compression.NewCodec(compression.DefaultLevel, 64*1024)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	zstdMagic := []byte{0x28, 0xb5, 0x2f, 0xfd}
	plain := append(append([]byte{}, zstdMagic...), []byte("not a seekable stream")...)
	be := backendtest.NewInMemory()
	be.Objects["file.zst"] = backendtest.Object{Data: plain}
	mgr := newFleet(t, locationsStore(t, nil, core.ErrDBUnavailable), map[string]backend.ObjectBackend{"b1": be}, &fleetOpts{Codec: codec})

	result, err := mgr.GetObject(context.Background(), "file.zst", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	if got, _ := io.ReadAll(result.Body); !bytes.Equal(got, plain) {
		t.Errorf("served %q, want the stored bytes %q", got, plain)
	}
}

// TestGetObject_DBUnavailable_ListFailureFailsThatBackend verifies a backend
// whose listing fails is treated like one whose GET failed.
func TestGetObject_DBUnavailable_ListFailureFailsThatBackend(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Objects["key"] = backendtest.Object{Data: []byte("data")}
	be.ListErr = io.ErrUnexpectedEOF
	mgr := newFleet(t, locationsStore(t, nil, core.ErrDBUnavailable), map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.GetObject(context.Background(), "key", ""); err == nil {
		t.Fatal("GetObject succeeded although the only backend could not list")
	}
}
