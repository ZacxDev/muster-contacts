package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The SQL ledger (design: "Owner scoping is one function … a ledger test
// lists every SQL statement that touches a person table and fails when one
// lacks owner = ?"). It reads this package's source, not its behaviour:
// every SQL string literal that reads, changes or deletes rows of a person
// table must filter `owner = ?` at least once per person table it names,
// and every INSERT into one must name the owner column. The exceptions are
// listed, and the list must match exactly (it fails when it grows OR
// shrinks).

// ownerlessStatements are the statements allowed to touch a person table
// without an owner filter, each by a prefix, with why.
var ownerlessStatements = map[string]string{
	"DELETE FROM idempotency WHERE at <= ?":                         "the sweep: every owner's keys past their lifetime",
	"DELETE FROM idempotency WHERE request_sha256 = '' AND at <= ?": "the sweep: every owner's tombstones past the outbox's 72 hours",
}

var (
	sqlStart  = regexp.MustCompile(`(?is)^\s*(SELECT|INSERT|UPDATE|DELETE|WITH)\b`)
	tableRef  = regexp.MustCompile(`(?i)\b(FROM|JOIN|UPDATE|INTO)\s+(` + strings.Join(PersonTables, "|") + `)\b`)
	ownerArg  = regexp.MustCompile(`\bowner = \?`)
	insertTo  = regexp.MustCompile(`(?is)^\s*INSERT\s+INTO\s+(\w+)\s*\(([^)]*)\)`)
	spaceRuns = regexp.MustCompile(`\s+`)
)

// sqlLiterals is every string literal in the package's non-test files that
// looks like a SQL statement.
func sqlLiterals(t *testing.T) []string {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err == nil && sqlStart.MatchString(v) {
				out = append(out, spaceRuns.ReplaceAllString(strings.TrimSpace(v), " "))
			}
			return true
		})
	}
	return out
}

// unscoped says what is wrong with a statement's owner scoping, "" when
// nothing is.
func unscoped(stmt string) string {
	if m := insertTo.FindStringSubmatch(stmt); m != nil {
		if slices.Contains(PersonTables, m[1]) {
			cols := strings.Split(m[2], ",")
			for i := range cols {
				cols[i] = strings.TrimSpace(cols[i])
			}
			if !slices.Contains(cols, "owner") {
				return "an INSERT into " + m[1] + " without the owner column"
			}
		}
		// An INSERT's own target is not a read; the rest of it (a
		// subquery) is checked below.
		stmt = stmt[len(m[0]):]
	}
	refs := len(tableRef.FindAllString(stmt, -1))
	if filters := len(ownerArg.FindAllString(stmt, -1)); filters < refs {
		return strconv.Itoa(refs) + " person-table references and " + strconv.Itoa(filters) + " owner = ? filters"
	}
	return ""
}

func TestEverySQLStatementIsOwnerScoped(t *testing.T) {
	stmts := sqlLiterals(t)
	touching, used := 0, map[string]bool{}
	for _, s := range stmts {
		if !tableRef.MatchString(s) {
			continue
		}
		touching++
		why := unscoped(s)
		if why == "" {
			continue
		}
		allowed := false
		for prefix := range ownerlessStatements {
			if strings.HasPrefix(s, prefix) {
				allowed, used[prefix] = true, true
			}
		}
		if !allowed {
			t.Errorf("SQL ledger: %s:\n\t%s", why, s)
		}
	}
	for prefix, why := range ownerlessStatements {
		if !used[prefix] {
			t.Errorf("SQL ledger: the exception %q (%s) matches no statement: remove it", prefix, why)
		}
	}
	// The positive control: the ledger saw the package's statements (a
	// parser that found none would pass every check).
	if touching < 40 {
		t.Fatalf("SQL ledger: %d statements touch a person table, want the package's 40+: the scan sees nothing?", touching)
	}
}

// The ledger's own controls: it flags a read with a missing filter, a join
// with one filter for two tables, and an insert without the owner column,
// and passes the scoped forms.
func TestTheSQLLedgerSeesAnUnscopedStatement(t *testing.T) {
	for stmt, bad := range map[string]bool{
		`SELECT name FROM contacts WHERE id = ?`:                                                                                       true,
		`SELECT name FROM contacts WHERE id = ? AND owner = ?`:                                                                         false,
		`SELECT x.contact_id FROM contact_emails x JOIN contacts c ON c.id = x.contact_id WHERE x.owner = ?`:                           true,
		`SELECT x.contact_id FROM contact_emails x JOIN contacts c ON c.owner = ? AND c.id = x.contact_id WHERE x.owner = ?`:           false,
		`INSERT INTO provenance (contact_id, kind, at) VALUES (?, ?, ?)`:                                                               true,
		`INSERT INTO provenance (contact_id, owner, kind, at) VALUES (?, ?, ?, ?)`:                                                     false,
		`DELETE FROM meetings WHERE owner = ? AND NOT EXISTS (SELECT 1 FROM meeting_links l WHERE l.meeting_id = meetings.meeting_id)`: true,
	} {
		if got := unscoped(stmt) != ""; got != bad {
			t.Errorf("%s: flagged %v, want %v", stmt, got, bad)
		}
	}
}
