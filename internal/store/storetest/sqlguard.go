// -------------------------------------------------------------------------------
// SQL Mutation Guard
//
// Author: Alex Freidah
//
// A copy row is only ever changed by a statement that names the exact copy it
// read, and a cleanup row only by the instance holding its claim. Both are
// properties of the SQL text, so they are checked there: every engine's SQL
// lives in Go string literals, the sqlc-generated constants for Postgres and
// the inline statements for SQLite, and one walk over a package's literals
// covers either. A new statement that mutates a guarded table without the
// guard fails until it is fixed or exempted with a reason.
// -------------------------------------------------------------------------------

package storetest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// MutationRule names a table and the column every UPDATE or DELETE on it must
// match in its WHERE clause.
type MutationRule struct {
	Table string
	Guard string
}

// CopyMutations requires a copy's storage_key, so a write based on a copy a
// newer write replaced matches no row. CleanupSettles requires the claim, so a
// worker that lost a cleanup row cannot settle it.
var (
	CopyMutations  = MutationRule{Table: "object_locations", Guard: "storage_key"}
	CleanupSettles = MutationRule{Table: "cleanup_queue", Guard: "claimed_at"}
)

// The reasons a statement may mutate a guarded table without its guard, shared
// by both engines' exemption lists.
const (
	ExemptWholeKey      = "deletes every copy of the key under the key lock"
	ExemptGuardedInCore = "core.DeleteObjectLocation re-reads the copy and compares storage_key under the key lock"
	ExemptBackendPurge  = "removes everything the backend holds"
	ExemptClaim         = "sets the claim the guard checks"
	ExemptPathSweep     = "reconcile removes rows by the path it found gone, whoever holds them"
)

// heldSQL is one string literal and the function or constant that holds it.
type heldSQL struct {
	holder string
	sql    string
}

// CheckMutations fails t for every statement in dir's Go files that updates or
// deletes rule.Table without naming rule.Guard after its WHERE, unless exempt
// maps the statement's holder to the reason it may. Holders are a function's
// name, qualified by its receiver type for a method, or a constant's name. An
// exemption that names no mutating statement fails too, so the list cannot
// outlive the code it excuses.
func CheckMutations(t testing.TB, dir string, rule MutationRule, exempt map[string]string) {
	t.Helper()
	mutates := regexp.MustCompile(`(?i)\b(?:UPDATE|DELETE\s+FROM)\s+` + rule.Table + `\b`)
	seen := make(map[string]bool)
	for _, lit := range literals(t, dir) {
		at := mutates.FindStringIndex(lit.sql)
		if at == nil {
			continue
		}
		seen[lit.holder] = true
		if strings.Contains(whereClause(lit.sql[at[1]:]), rule.Guard) {
			continue
		}
		if _, ok := exempt[lit.holder]; !ok {
			t.Errorf("%s mutates %s without matching %s:\n%s", lit.holder, rule.Table, rule.Guard, lit.sql)
		}
	}
	for holder := range exempt {
		if !seen[holder] {
			t.Errorf("exemption %s names no statement that mutates %s", holder, rule.Table)
		}
	}
}

// whereClause returns the WHERE clause of the statement that rest begins
// inside, up to its RETURNING list, or "" when it has none. A column named
// only in what the statement returns does not choose the rows it changes.
func whereClause(rest string) string {
	upper := strings.ToUpper(rest)
	where := strings.Index(upper, "WHERE")
	if where < 0 {
		return ""
	}
	if end := strings.Index(upper[where:], "RETURNING"); end >= 0 {
		return rest[where : where+end]
	}
	return rest[where:]
}

// literals returns every string literal in dir's non-test Go files with its
// holder.
func literals(t testing.TB, dir string) []heldSQL {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("list Go files in %s: %v", dir, err)
	}
	var out []heldSQL
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			out = append(out, declLiterals(decl)...)
		}
	}
	return out
}

// declLiterals returns the string literals one declaration holds: those in a
// function's body under its name, and each constant's or variable's value
// under its own.
func declLiterals(decl ast.Decl) []heldSQL {
	var out []heldSQL
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Body != nil {
			out = stringLiterals(out, funcHolder(d), d.Body)
		}
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, value := range vs.Values {
				out = stringLiterals(out, vs.Names[min(i, len(vs.Names)-1)].Name, value)
			}
		}
	}
	return out
}

// stringLiterals appends every string literal under node to out, held by
// holder.
func stringLiterals(out []heldSQL, holder string, node ast.Node) []heldSQL {
	ast.Inspect(node, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if s, err := strconv.Unquote(lit.Value); err == nil {
			out = append(out, heldSQL{holder: holder, sql: s})
		}
		return true
	})
	return out
}

// funcHolder names a function, qualified by its receiver type for a method.
func funcHolder(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	recv := d.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if ident, ok := recv.(*ast.Ident); ok {
		return ident.Name + "." + d.Name.Name
	}
	return d.Name.Name
}
