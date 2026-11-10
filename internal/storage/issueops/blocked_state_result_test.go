package issueops

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func newBlockedStateResultMock(t *testing.T) (sqlmock.Sqlmock, DBTX) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	})
	return mock, db
}

// expectBatchExogeneityRead is the batch-scoped exogeneity read every batched
// pass now runs before its statements: first the parent-kind guard, and then
// one read per kind the batch actually names. Answering the guard with no
// parents of either kind is also the pin on the guard itself — a batch with no
// parent-child row must not run either read.
//
// Ordered before the execs because sqlmock is ordered and the runner reads
// once per batch, then binds the ids into the mark and the unmark's read.
func expectBatchExogeneityRead(mock sqlmock.Sqlmock, depTable string) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(d.depends_on_issue_id), COUNT(d.depends_on_wisp_id)")).
		WillReturnRows(sqlmock.NewRows([]string{"issue_parents", "wisp_parents"}).AddRow(0, 0))
}

// expectUnmarkRead expects the batched unmark's read of the scoped
// should-be-blocked union (readShouldBeBlockedInTx), between the mark and the
// unmark, answering it with stillBlocked.
func expectUnmarkRead(mock sqlmock.Sqlmock, depTable string, stillBlocked ...string) {
	rows := sqlmock.NewRows([]string{"issue_id"})
	for _, id := range stillBlocked {
		rows.AddRow(id)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT /*+ JOIN_ORDER(d, t) LOOKUP_JOIN(d, t) */ d.issue_id FROM " + depTable + " d")).
		WillReturnRows(rows)
}

// expectOwnEdgesProbe expects planRecomputeInTx's probe of depTable and
// answers that every id in withEdges has a dependency row of its own.
func expectOwnEdgesProbe(mock sqlmock.Sqlmock, depTable string, withEdges ...string) {
	rows := sqlmock.NewRows([]string{"issue_id"})
	for _, id := range withEdges {
		rows.AddRow(id)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT issue_id FROM " + depTable + " WHERE issue_id IN")).
		WillReturnRows(rows)
}

func expectBlockedStatePass(mock sqlmock.Sqlmock, table, alias string, mark, unmark driver.Result) {
	expectBatchExogeneityRead(mock, blockedSpecFor(table).depTable)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE " + table + " " + alias + " SET " + alias + ".is_blocked = 1")).
		WillReturnResult(mark)
	expectUnmarkRead(mock, blockedSpecFor(table).depTable)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE " + table + " " + alias + " SET " + alias + ".is_blocked = 0")).
		WillReturnResult(unmark)
}

func blockedSpecFor(table string) blockedTableSpec {
	if table == "wisps" {
		return wispsBlockedSpec
	}
	return issuesBlockedSpec
}

func TestRecomputeIsBlockedInTxWithResult(t *testing.T) {
	t.Run("issue rows changed", func(t *testing.T) {
		mock, db := newBlockedStateResultMock(t)
		expectBlockedStatePass(mock, "issues", "i", sqlmock.NewResult(0, 1), sqlmock.NewResult(0, 0))
		expectBlockedStatePass(mock, "issues", "i", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 0))

		result, err := RecomputeIsBlockedInTxWithResult(context.Background(), db, []string{"issue-1"}, nil)
		if err != nil {
			t.Fatalf("RecomputeIsBlockedInTxWithResult: %v", err)
		}
		if !result.IssueRowsChanged || result.WispRowsChanged {
			t.Fatalf("result = %+v, want issue change only", result)
		}
	})

	t.Run("wisp rows changed", func(t *testing.T) {
		mock, db := newBlockedStateResultMock(t)
		expectBlockedStatePass(mock, "wisps", "w", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 1))
		expectBlockedStatePass(mock, "wisps", "w", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 0))

		result, err := RecomputeIsBlockedInTxWithResult(context.Background(), db, nil, []string{"wisp-1"})
		if err != nil {
			t.Fatalf("RecomputeIsBlockedInTxWithResult: %v", err)
		}
		if result.IssueRowsChanged || !result.WispRowsChanged {
			t.Fatalf("result = %+v, want wisp change only", result)
		}
	})

	t.Run("no rows changed", func(t *testing.T) {
		mock, db := newBlockedStateResultMock(t)
		expectBlockedStatePass(mock, "issues", "i", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 0))
		expectBlockedStatePass(mock, "wisps", "w", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 0))

		result, err := RecomputeIsBlockedInTxWithResult(
			context.Background(), db, []string{"issue-1"}, []string{"wisp-1"},
		)
		if err != nil {
			t.Fatalf("RecomputeIsBlockedInTxWithResult: %v", err)
		}
		if result.IssueRowsChanged || result.WispRowsChanged {
			t.Fatalf("result = %+v, want no changes", result)
		}
	})
}

func TestRunMarkUnmarkBatchedInTxPropagatesRowsAffectedErrors(t *testing.T) {
	sentinel := errors.New("rows affected unavailable")
	for _, phase := range []string{"mark", "unmark"} {
		t.Run(phase, func(t *testing.T) {
			mock, db := newBlockedStateResultMock(t)
			expectBatchExogeneityRead(mock, "dependencies")
			if phase == "mark" {
				mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 1")).
					WillReturnResult(sqlmock.NewErrorResult(sentinel))
			} else {
				mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 1")).
					WillReturnResult(sqlmock.NewResult(0, 0))
				expectUnmarkRead(mock, "dependencies")
				mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 0")).
					WillReturnResult(sqlmock.NewErrorResult(sentinel))
			}

			_, err := runMarkUnmarkBatchedInTx(
				context.Background(), db, issuesBlockedSpec, []string{"issue-1"},
			)
			if !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want rows-affected error", err)
			}
		})
	}
}

// TestRunMarkUnmarkBatchedInTxReadsBeforeTheUnmark pins the unmark's
// statement sequence: the union is read AFTER the mark, and the unmark binds
// what it returned instead of carrying the union as a subquery. The anchored
// patterns match the whole unmark, so a union spliced back in fails them.
func TestRunMarkUnmarkBatchedInTxReadsBeforeTheUnmark(t *testing.T) {
	const unmarkHead = `^\s*UPDATE issues i SET i\.is_blocked = 0, i\.updated_at = i\.updated_at\s+` +
		`WHERE i\.id IN \(\?,\?\)\s+AND i\.is_blocked = 1`
	for _, tc := range []struct {
		name         string
		stillBlocked []string
		pattern      string
		args         []driver.Value
	}{
		{"read set bound", []string{"issue-2"},
			unmarkHead + `\s+AND \( i\.status = 'closed' OR i\.status = 'pinned'\s+OR i\.id NOT IN \(\?\) \)\s*$`,
			[]driver.Value{"issue-1", "issue-2", "issue-2"}},
		{"empty read clears every flagged row", nil, unmarkHead + `\s*$`,
			[]driver.Value{"issue-1", "issue-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, db := newBlockedStateResultMock(t)
			expectBatchExogeneityRead(mock, "dependencies")
			mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 1")).
				WillReturnResult(sqlmock.NewResult(0, 1))
			expectUnmarkRead(mock, "dependencies", tc.stillBlocked...)
			mock.ExpectExec(tc.pattern).WithArgs(tc.args...).WillReturnResult(sqlmock.NewResult(0, 1))

			n, err := runMarkUnmarkBatchedInTx(context.Background(), db, issuesBlockedSpec, []string{"issue-1", "issue-2"})
			if err != nil {
				t.Fatalf("runMarkUnmarkBatchedInTx: %v", err)
			}
			if n != 2 {
				t.Fatalf("changed = %d, want 2", n)
			}
		})
	}
}

// TestRunMarkUnmarkBatchedInTxReadBindsScopeAndExogeneity pins the unmark
// read's whole statement and argument list over two chunks, each with its own
// non-empty exogeneity set: every leg confined to that chunk's ids, and the
// chunk's explained parents excluded from the two parent-child legs. Dropping
// the batch scope or the exclusions from the read fails the anchored SQL; the
// arguments are spelled out by hand, in the text order the legs consume them.
func TestRunMarkUnmarkBatchedInTxReadBindsScopeAndExogeneity(t *testing.T) {
	ids := make([]string, queryBatchSize+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("issue-%03d", i)
	}
	chunks := []struct {
		ids          []string
		explained    subtreeExplainedParents
		stillBlocked string
	}{
		{ids[:queryBatchSize], subtreeExplainedParents{issueParents: []string{"parent-1"}, wispParents: []string{"wisp-parent-1"}}, "issue-007"},
		{ids[queryBatchSize:], subtreeExplainedParents{issueParents: []string{"parent-2"}}, "issue-200"},
	}

	mock, db := newBlockedStateResultMock(t)
	for _, c := range chunks {
		hasWisp, wispParents := len(c.explained.wispParents) > 0, 0
		if hasWisp {
			wispParents = 1
		}
		mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(d.depends_on_issue_id), COUNT(d.depends_on_wisp_id)")).
			WillReturnRows(sqlmock.NewRows([]string{"i", "w"}).AddRow(1, wispParents))
		mock.ExpectQuery(`^WITH`).WillReturnRows(sqlmock.NewRows([]string{"pid"}).AddRow(c.explained.issueParents[0]))
		climbs := []string{"dependencies", "dependencies"}
		if hasWisp {
			mock.ExpectQuery(`^WITH`).WillReturnRows(sqlmock.NewRows([]string{"pid"}).AddRow(c.explained.wispParents[0]))
			climbs = append(climbs, "wisp_dependencies", "wisp_dependencies")
		}
		// The ancestry prune climbs from each explained parent and finds
		// nothing blocked above it, so the set survives whole.
		for _, depTable := range climbs {
			mock.ExpectQuery(regexp.QuoteMeta("SELECT /*+ JOIN_ORDER(d, p) LOOKUP_JOIN(d, p) */ d.issue_id, p.id FROM " + depTable)).
				WillReturnRows(sqlmock.NewRows([]string{"issue_id", "id"}))
		}
		mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 1")).WillReturnResult(sqlmock.NewResult(0, 0))

		placeholders, batch := buildSQLInClause(c.ids)
		read, _, err := expandBatchTemplate(
			shouldBeBlockedIDsUnionScopedPrecomputedSQL("dependencies", batchScopeSQL, c.explained),
			placeholders, batch, c.explained.args())
		if err != nil {
			t.Fatalf("expandBatchTemplate: %v", err)
		}
		var readArgs []driver.Value
		for leg := 1; leg <= 5; leg++ {
			for _, id := range c.ids {
				readArgs = append(readArgs, id)
			}
			if leg == 3 {
				readArgs = append(readArgs, c.explained.issueParents[0])
			}
			if leg == 4 && hasWisp {
				readArgs = append(readArgs, c.explained.wispParents[0])
			}
		}
		mock.ExpectQuery("^" + regexp.QuoteMeta(stripWhitespace(read)) + "$").WithArgs(readArgs...).
			WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow(c.stillBlocked))

		unmarkArgs := append(append([]driver.Value{}, readArgs[:len(c.ids)]...), c.stillBlocked)
		mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 0")).WithArgs(unmarkArgs...).
			WillReturnResult(sqlmock.NewResult(0, 0))
	}

	if _, err := runMarkUnmarkBatchedInTx(context.Background(), db, issuesBlockedSpec, ids); err != nil {
		t.Fatalf("runMarkUnmarkBatchedInTx: %v", err)
	}
}

// stripWhitespace collapses runs of whitespace the way sqlmock does before it
// matches, so a whole statement can be anchored.
func stripWhitespace(q string) string {
	return regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(q), " ")
}

// TestRecomputeIsBlockedSkipsUnionForIDsWithoutEdges pins the no-edge
// shortcut a create asks for (the single-id cases above pin that nobody else
// pays its probe): an id with no dependency row of its own cannot be in the scoped
// should-be-blocked union, so it never reaches the union statements. It is
// cleared by one plain unmark on the first pass, after the chunk's union
// statements (which must see its pre-pass is_blocked, as the chunk's own
// unmark once did), and not revisited on later passes.
func TestRecomputeIsBlockedSkipsUnionForIDsWithoutEdges(t *testing.T) {
	mock, db := newBlockedStateResultMock(t)
	expectOwnEdgesProbe(mock, "dependencies", "issue-2")
	// Pass 1: the union statements over the id with edges, then the plain
	// unmark over the edgeless ids.
	expectBatchExogeneityRead(mock, "dependencies")
	mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 1")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectUnmarkRead(mock, "dependencies")
	mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 0")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE issues t SET t.is_blocked = 0, t.updated_at = t.updated_at")).
		WithArgs("issue-1", "issue-3").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Pass 2: only the id with edges.
	expectBatchExogeneityRead(mock, "dependencies")
	mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 1")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectUnmarkRead(mock, "dependencies")
	mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 0")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	result, err := recomputeIsBlockedInTxWithResult(context.Background(), db, []string{"issue-1", "issue-2", "issue-3"}, nil, recomputeOptions{splitEdgeless: true})
	if err != nil {
		t.Fatalf("RecomputeIsBlockedInTxWithResult: %v", err)
	}
	if !result.IssueRowsChanged || result.WispRowsChanged {
		t.Fatalf("result = %+v, want issue change only", result)
	}
}

// TestRecomputeIsBlockedSettlesClosedRowWithoutUnion pins the shortcut a close
// or a status change to closed/pinned takes for the row it just wrote: neither
// template can mark a closed row and the unmark template clears it
// unconditionally, so the row is cleared by one plain unmark with no probe and
// no union statements, whatever its edges. The row's dependents are not
// settled by it and still ride the union statements on every pass.
func TestRecomputeIsBlockedSettlesClosedRowWithoutUnion(t *testing.T) {
	t.Run("row alone", func(t *testing.T) {
		mock, db := newBlockedStateResultMock(t)
		mock.ExpectExec(regexp.QuoteMeta("UPDATE wisps t SET t.is_blocked = 0, t.updated_at = t.updated_at")).
			WithArgs("wisp-1").
			WillReturnResult(sqlmock.NewResult(0, 0))

		result, err := recomputeIsBlockedInTxWithResult(context.Background(), db, nil, []string{"wisp-1"}, settledRowRecompute("wisp-1", true))
		if err != nil {
			t.Fatalf("recomputeIsBlockedInTxWithResult: %v", err)
		}
		if result.IssueRowsChanged || result.WispRowsChanged {
			t.Fatalf("result = %+v, want no changes", result)
		}
	})

	t.Run("row and its dependents", func(t *testing.T) {
		mock, db := newBlockedStateResultMock(t)
		// Pass 1: the union statements over the dependents only, then the
		// plain unmark over the closed row.
		expectBatchExogeneityRead(mock, "dependencies")
		mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 1")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		expectUnmarkRead(mock, "dependencies")
		mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 0")).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta("UPDATE issues t SET t.is_blocked = 0, t.updated_at = t.updated_at")).
			WithArgs("issue-closed").
			WillReturnResult(sqlmock.NewResult(0, 1))
		// Pass 2: only the dependents.
		expectBatchExogeneityRead(mock, "dependencies")
		mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 1")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		expectUnmarkRead(mock, "dependencies")
		mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 0")).
			WillReturnResult(sqlmock.NewResult(0, 0))

		result, err := recomputeIsBlockedInTxWithResult(context.Background(), db,
			[]string{"issue-waiter", "issue-closed"}, nil, settledRowRecompute("issue-closed", false))
		if err != nil {
			t.Fatalf("recomputeIsBlockedInTxWithResult: %v", err)
		}
		if !result.IssueRowsChanged || result.WispRowsChanged {
			t.Fatalf("result = %+v, want issue change only", result)
		}
	})

	t.Run("hint names the other plane", func(t *testing.T) {
		// A settled wisp id says nothing about an issues row that shares the
		// id: the issues row keeps the full recompute.
		mock, db := newBlockedStateResultMock(t)
		expectBlockedStatePass(mock, "issues", "i", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 0))

		if _, err := recomputeIsBlockedInTxWithResult(context.Background(), db,
			[]string{"shared-1"}, nil, settledRowRecompute("shared-1", true)); err != nil {
			t.Fatalf("recomputeIsBlockedInTxWithResult: %v", err)
		}
	})
}

// TestRecomputeSettledShortcutHonorsFastPathSwitch pins that the settled-row
// shortcut is one of the fast paths DisableCreateFastPathsForTest turns off, so
// the per-row equivalence tests compare it against the full recompute.
func TestRecomputeSettledShortcutHonorsFastPathSwitch(t *testing.T) {
	restore := DisableCreateFastPathsForTest()
	defer restore()
	mock, db := newBlockedStateResultMock(t)
	expectBlockedStatePass(mock, "wisps", "w", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 0))

	if _, err := recomputeIsBlockedInTxWithResult(context.Background(), db,
		nil, []string{"wisp-1"}, settledRowRecompute("wisp-1", true)); err != nil {
		t.Fatalf("recomputeIsBlockedInTxWithResult: %v", err)
	}
}
