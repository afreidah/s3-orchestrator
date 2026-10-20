// -------------------------------------------------------------------------------
// Advisory Lock ID Tests
//
// Author: Alex Freidah
//
// Two services sharing a lock ID skip each other's ticks without any error to
// show for it. The IDs are read from locks.go itself, so a new constant is
// checked as soon as it is declared.
// -------------------------------------------------------------------------------

package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestLockIDs_Unique verifies every int64 advisory lock ID declared in
// locks.go is distinct. The claim lock space is a separate namespace.
func TestLockIDs_Unique(t *testing.T) {
	t.Parallel()
	ids := declaredLockIDs(t)
	if len(ids) < 2 {
		t.Fatalf("found %d lock IDs in locks.go, want every service's", len(ids))
	}
	owner := map[string]string{}
	for _, id := range ids {
		if prev, dup := owner[id.value]; dup {
			t.Errorf("%s and %s share lock ID %s", prev, id.name, id.value)
		}
		owner[id.value] = id.name
	}
}

// lockID is one int64 constant declared in locks.go and its literal value.
type lockID struct {
	name, value string
}

// declaredLockIDs parses locks.go and returns every int64 constant with an
// integer literal value, in declaration order.
func declaredLockIDs(t *testing.T) []lockID {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "locks.go", nil, 0)
	if err != nil {
		t.Fatalf("parse locks.go: %v", err)
	}
	var ids []lockID
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if typ, ok := vs.Type.(*ast.Ident); !ok || typ.Name != "int64" {
			return false
		}
		for i, name := range vs.Names {
			if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.INT {
				ids = append(ids, lockID{name: name.Name, value: lit.Value})
			}
		}
		return false
	})
	return ids
}
