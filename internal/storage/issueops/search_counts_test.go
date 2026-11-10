package issueops

import (
	"context"
	"database/sql/driver"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

func TestSearchIssuesWithCountsAppliesLimitToEachSourceQuery(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(`SELECT 1 FROM wisp_dependencies LIMIT 1`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`(?s)FROM issues i.*ORDER BY i\.priority ASC, i\.created_at DESC, i\.id ASC\s+LIMIT 3`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(`SELECT 1 FROM wisps LIMIT 1`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`(?s)FROM wisps i.*ORDER BY i\.priority ASC, i\.created_at DESC, i\.id ASC\s+LIMIT 3`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	got, err := SearchIssuesWithCountsInTx(context.Background(), tx, "", types.IssueFilter{Limit: 3})
	if err != nil {
		t.Fatalf("SearchIssuesWithCountsInTx: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("SearchIssuesWithCountsInTx returned %d rows, want none", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// Regression for bd-6dnrw.42: SkipWisps must suppress the wisps probe and
// merge, matching SearchIssuesInTx and the domain/db counts path. sqlmock
// rejects any query without a matching expectation, so only the
// wisp_dependencies probe and the issues query may run.
func TestSearchIssuesWithCountsHonorsSkipWisps(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(`SELECT 1 FROM wisp_dependencies LIMIT 1`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`(?s)FROM issues i.*ORDER BY i\.priority ASC, i\.created_at DESC, i\.id ASC`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	got, err := SearchIssuesWithCountsInTx(context.Background(), tx, "", types.IssueFilter{SkipWisps: true})
	if err != nil {
		t.Fatalf("SearchIssuesWithCountsInTx: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("SearchIssuesWithCountsInTx returned %d rows, want none", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// Regression for bd-6dnrw.43: SortBy/SortDesc must reach the per-table SQL
// ORDER BY; otherwise LIMIT is applied under priority order and the wrong
// row SET survives (not just the wrong order).
func TestSearchIssuesWithCountsPushesSortIntoSQL(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(`SELECT 1 FROM wisp_dependencies LIMIT 1`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`(?s)FROM issues i.*ORDER BY i\.created_at DESC, i\.id ASC\s+LIMIT 2`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(`SELECT 1 FROM wisps LIMIT 1`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`(?s)FROM wisps i.*ORDER BY i\.created_at DESC, i\.id ASC\s+LIMIT 2`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	filter := types.IssueFilter{SortBy: "created", Limit: 2}
	if _, err := SearchIssuesWithCountsInTx(context.Background(), tx, "", filter); err != nil {
		t.Fatalf("SearchIssuesWithCountsInTx: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// Regression for bd-6dnrw.43 (merge half): when issues and wisps results are
// merged before the limit cut, the in-memory sort must follow the requested
// SortBy, or the surviving row set reverts to priority order.
func TestFinishSearchIssuesWithCountsTruncatesByRequestedSort(t *testing.T) {
	t.Parallel()

	at := func(day int) time.Time {
		return time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC)
	}
	iwc := func(id string, priority int, created time.Time) *types.IssueWithCounts {
		return &types.IssueWithCounts{Issue: &types.Issue{ID: id, Priority: priority, CreatedAt: created}}
	}
	// P0 but oldest; the two newest are low priority.
	items := []*types.IssueWithCounts{
		iwc("bd-old-p0", 0, at(1)),
		iwc("bd-new-p3", 3, at(9)),
		iwc("bd-mid-p2", 2, at(5)),
		iwc("bd-newest-p4", 4, at(10)),
	}

	got, err := finishSearchIssuesWithCounts(items, types.IssueFilter{SortBy: "created", Limit: 2})
	if err != nil {
		t.Fatalf("sort=created limit=2: unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].Issue.ID != "bd-newest-p4" || got[1].Issue.ID != "bd-new-p3" {
		ids := make([]string, len(got))
		for i, g := range got {
			ids[i] = g.Issue.ID
		}
		t.Fatalf("sort=created limit=2 kept %v, want [bd-newest-p4 bd-new-p3]", ids)
	}

	// SortDesc flips the per-key default direction: created becomes ASC.
	got, err = finishSearchIssuesWithCounts(items, types.IssueFilter{SortBy: "created", SortDesc: true, Limit: 2})
	if err != nil {
		t.Fatalf("sort=created desc limit=2: unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].Issue.ID != "bd-old-p0" || got[1].Issue.ID != "bd-mid-p2" {
		ids := make([]string, len(got))
		for i, g := range got {
			ids[i] = g.Issue.ID
		}
		t.Fatalf("sort=created desc limit=2 kept %v, want [bd-old-p0 bd-mid-p2]", ids)
	}

	// Default (no SortBy) keeps the historical priority/created/id order.
	got, err = finishSearchIssuesWithCounts(items, types.IssueFilter{Limit: 2})
	if err != nil {
		t.Fatalf("default sort limit=2: unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].Issue.ID != "bd-old-p0" || got[1].Issue.ID != "bd-mid-p2" {
		ids := make([]string, len(got))
		for i, g := range got {
			ids[i] = g.Issue.ID
		}
		t.Fatalf("default sort limit=2 kept %v, want [bd-old-p0 bd-mid-p2]", ids)
	}
}

// liteCountsRows builds a Lite counts result (ScanReadyWorkRowWithCounts with
// CountsHydration{Lite: true}) holding one row per id, in the order given.
func liteCountsRows(ids ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows(append(liteColumnNames(), "labels_json", "dep_count", "rdep_count", "comment_count", "parent_id", "deps_json"))
	for _, id := range ids {
		rows.AddRow(append(liteMergeRow(id, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)), nil, 0, 0, 0, nil, nil)...)
	}
	return rows
}

func iwcIDs(items []*types.IssueWithCounts) []string {
	out := make([]string, 0, len(items))
	for _, iwc := range items {
		out = append(out, iwc.Issue.ID)
	}
	return out
}

// A filtered counts search selects its page ids from the main table alone and
// hydrates them through the by-IDs form, whose every aggregate is bounded by
// issue_id IN (...). The result keeps the id query's order, not the order the
// hydration statement emits, and an id the hydration no longer finds is skipped.
func TestSearchCountsTwoPhaseHydratesByIDsInIDQueryOrder(t *testing.T) {
	t.Parallel()

	var captured []string
	mock, tx := beginCapturingMockTx(t, &captured)
	mock.ExpectQuery(`^SELECT i\.id FROM issues i WHERE status = \? ORDER BY i\.priority ASC, i\.created_at DESC, i\.id ASC LIMIT 4$`).
		WithArgs("open").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("bd-c").AddRow("bd-a").AddRow("bd-gone").AddRow("bd-b"))
	mock.ExpectQuery(`issue_id IN \(`).
		WillReturnRows(liteCountsRows("bd-a", "bd-b", "bd-c"))

	got, err := SearchCountsTwoPhaseInTx(context.Background(), tx, IssuesFilterTables, "WHERE status = ?",
		"ORDER BY i.priority ASC, i.created_at DESC, i.id ASC", "LIMIT 4", []any{"open"}, true, liteHydration)
	if err != nil {
		t.Fatalf("SearchCountsTwoPhaseInTx: %v", err)
	}
	if ids := iwcIDs(got); strings.Join(ids, ",") != "bd-c,bd-a,bd-b" {
		t.Fatalf("rows = %v, want the id query's order [bd-c bd-a bd-b]", ids)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
	if len(captured) != 2 || strings.Contains(captured[0], "JOIN") {
		t.Fatalf("want a main-table-only id query and one hydration chunk, got:\n%s", strings.Join(captured, "\n---\n"))
	}
	// labels, dc, cc, pc and d each bound by issue_id; rc by its target; the driver by i.id.
	hydrate := captured[1]
	if strings.Count(hydrate, "issue_id IN (") != 5 || !strings.Contains(hydrate, "depends_on_external) IN (") ||
		!strings.Contains(hydrate, "WHERE i.id IN (") || strings.Contains(hydrate, "SELECT i.*") {
		t.Errorf("hydration is not the by-IDs form:\n%s", hydrate)
	}
}

// An id query that matches nothing ends the search: no hydration statement is
// sent. sqlmock refuses any statement it was not primed for.
func TestSearchCountsTwoPhaseEmptyPageSendsNoHydration(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(`^SELECT i\.id FROM wisps i WHERE status = \?`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	got, err := SearchCountsTwoPhaseInTx(context.Background(), tx, WispsFilterTables, "WHERE status = ?", "", "", []any{"open"}, true, liteHydration)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %d rows, err %v; want none", len(got), err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// Unfiltered and unbounded, the search keeps the single predicate-form
// statement, since it reads every row anyway. sqlmock refuses an id query here.
func TestSearchCountsTwoPhaseUnfilteredUnboundedKeepsOneStatement(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(`(?s)^\s*SELECT i\..*FROM issues i\s.*LEFT JOIN`).WillReturnRows(liteCountsRows("bd-a"))

	if _, err := SearchCountsTwoPhaseInTx(context.Background(), tx, IssuesFilterTables, "", "ORDER BY i.id ASC", "", nil, true, liteHydration); err != nil {
		t.Fatalf("SearchCountsTwoPhaseInTx: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// A page larger than sqlbuild.QueryBatchSize is hydrated in chunks. Each chunk
// binds exactly its own ids once per placeholder site (labels, dc, rc, rc-wisp,
// cc, pc, d, driver), and every row comes back in id-query order.
func TestSearchCountsTwoPhaseChunksTheHydration(t *testing.T) {
	t.Parallel()

	var captured []string
	mock, tx := beginCapturingMockTx(t, &captured)
	ids := make([]string, sqlbuild.QueryBatchSize+1)
	page := sqlmock.NewRows([]string{"id"})
	for i := range ids {
		ids[i] = fmt.Sprintf("bd-%03d", len(ids)-1-i) // descending, unlike the hydration's order
		page.AddRow(ids[i])
	}
	mock.ExpectQuery(`^SELECT i\.id FROM issues i`).WillReturnRows(page)
	for _, chunk := range [][]string{ids[:sqlbuild.QueryBatchSize], ids[sqlbuild.QueryBatchSize:]} {
		var args []driver.Value
		for range 8 {
			for _, id := range chunk {
				args = append(args, id)
			}
		}
		mock.ExpectQuery(`issue_id IN \(`).WithArgs(args...).WillReturnRows(liteCountsRows(slices.Sorted(slices.Values(chunk))...))
	}

	got, err := SearchCountsTwoPhaseInTx(context.Background(), tx, IssuesFilterTables, "WHERE status = ?", "", "", []any{"open"}, true, liteHydration)
	if err != nil {
		t.Fatalf("SearchCountsTwoPhaseInTx: %v", err)
	}
	if strings.Join(iwcIDs(got), ",") != strings.Join(ids, ",") {
		t.Fatalf("rows = %v, want all %d ids in id-query order", iwcIDs(got), len(ids))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
	for i, n := range []int{sqlbuild.QueryBatchSize, 1} {
		if c := strings.Count(captured[i+1], "?"); c != 8*n {
			t.Errorf("chunk %d has %d placeholders, want %d", i, c, 8*n)
		}
	}
}

var liteHydration = sqlbuild.CountsHydration{Lite: true}
