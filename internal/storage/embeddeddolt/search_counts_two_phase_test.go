//go:build cgo

package embeddeddolt_test

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/domain/db"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// TestSearchCountsTwoPhaseMatchesPredicateForm runs each filter as the single
// predicate-form statement and through issueops.SearchCountsTwoPhaseInTx, the
// body both counts seams share: rows, order and hydrated values must match, and
// both seams end to end must answer the same page.
func TestSearchCountsTwoPhaseMatchesPredicateForm(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "tp")
	ctx := t.Context()
	for i, title := range []string{"alpha root", "beta child", "gamma blocker", "delta closed", "epsilon leaf"} {
		must(t, te.store.CreateIssue(ctx, &types.Issue{ID: fmt.Sprintf("tp-%d", i), Title: title, Status: types.StatusOpen, Priority: i % 3, IssueType: types.TypeTask}, "tester"))
	}
	must(t, te.store.CloseIssue(ctx, "tp-3", "done", "tester", "s1"))
	for _, l := range [][2]string{{"tp-0", "alpha"}, {"tp-0", "shared"}, {"tp-2", "alpha"}, {"tp-4", "shared"}} {
		must(t, te.store.AddLabel(ctx, l[0], l[1], "tester"))
	}
	for _, d := range []types.Dependency{{IssueID: "tp-1", DependsOnID: "tp-0", Type: types.DepParentChild},
		{IssueID: "tp-1", DependsOnID: "tp-2", Type: types.DepBlocks}, {IssueID: "tp-4", DependsOnID: "tp-2", Type: types.DepBlocks},
		{IssueID: "tp-0", DependsOnID: "tp-2", Type: types.DepBlocks}} {
		must(t, te.store.AddDependency(ctx, &d, "tester"))
	}
	for _, id := range []string{"tp-0", "tp-0", "tp-2"} {
		must(t, te.store.AddComment(ctx, id, "tester", "note"))
	}
	raw, cleanup, err := embeddeddolt.OpenSQL(ctx, te.dataDir, te.database, "main")
	must(t, err)
	defer func() { _ = cleanup() }()

	open := []types.Status{types.StatusOpen}
	for _, tc := range []struct {
		name, query string
		filter      types.IssueFilter
	}{
		{"limit only", "", types.IssueFilter{Limit: 2}},
		{"status, created, limit", "", types.IssueFilter{Statuses: open, SortBy: "created", Limit: 3}},
		{"title desc, limit", "", types.IssueFilter{SortBy: "title", SortDesc: true, Limit: 4}},
		{"label", "", types.IssueFilter{Labels: []string{"alpha"}}},
		{"substring", "ta", types.IssueFilter{}},
		{"go-side id sort", "", types.IssueFilter{Statuses: open, SortBy: "id", SortDesc: true, Limit: 2}},
		{"skip labels", "", types.IssueFilter{Statuses: open, SkipLabels: true, Limit: 3}},
		{"skip counts", "", types.IssueFilter{Labels: []string{"shared"}, SkipCounts: true}},
		{"lite", "", types.IssueFilter{Statuses: open, Lite: true}},
		{"matches nothing", "", types.IssueFilter{Labels: []string{"no-such-label"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.filter
			clauses, args, err := sqlbuild.BuildIssueFilterClauses(tc.query, f, sqlbuild.IssuesFilterTables)
			must(t, err)
			whereSQL, limitSQL, goSide := "", "", sqlbuild.IsGoSideSort(f.SortBy)
			if len(clauses) > 0 {
				whereSQL = "WHERE " + strings.Join(clauses, " AND ")
			}
			if f.Limit > 0 && !goSide {
				limitSQL = fmt.Sprintf("LIMIT %d", f.Limit)
			}
			orderBy := sqlbuild.OrderBy(f.SortBy, f.SortDesc, "i")
			hyd := sqlbuild.CountsHydration{SkipLabels: f.SkipLabels, SkipCounts: f.SkipCounts, Lite: f.Lite}

			predicateSQL, _ := sqlbuild.SearchCountsSQL(sqlbuild.IssuesFilterTables, nil, whereSQL, orderBy, limitSQL, true, hyd)
			rows, err := raw.QueryContext(ctx, predicateSQL, args...)
			must(t, err)
			var want []*types.IssueWithCounts
			for rows.Next() {
				iwc, err := issueops.ScanReadyWorkRowWithCounts(rows, hyd)
				must(t, err)
				want = append(want, iwc)
			}
			must(t, rows.Err())
			got, err := issueops.SearchCountsTwoPhaseInTx(ctx, raw, sqlbuild.IssuesFilterTables, whereSQL, orderBy, limitSQL, args, true, hyd)
			must(t, err)
			if tc.name != "matches nothing" && len(want) == 0 {
				t.Fatal("fixture must give the predicate form rows to compare")
			}
			for _, rs := range [][]*types.IssueWithCounts{want, got} {
				for _, iwc := range rs { // deps_json element order is not part of the contract
					deps := iwc.Issue.Dependencies
					sort.Slice(deps, func(i, j int) bool { return deps[i].DependsOnID < deps[j].DependsOnID })
				}
				if goSide {
					sort.Slice(rs, func(i, j int) bool { return sqlbuild.LessID(rs[i].Issue.ID, rs[j].Issue.ID, f.SortDesc) })
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("two-phase rows differ from the predicate form:\n got %v\nwant %v", idsOf(got), idsOf(want))
			}

			if f.Limit > 0 && len(want) > f.Limit {
				want = want[:f.Limit]
			}
			viaStore, err := te.store.SearchIssuesWithCounts(ctx, tc.query, f)
			must(t, err)
			viaRepo, err := db.NewIssueSQLRepository(raw).SearchAcrossIssuesAndWispsWithCounts(ctx, tc.query, f)
			must(t, err)
			for seam, rs := range map[string][]*types.IssueWithCounts{"store": viaStore, "domain/db": viaRepo.Items} {
				if !reflect.DeepEqual(idsOf(rs), idsOf(want)) {
					t.Errorf("%s seam page = %v, want %v", seam, idsOf(rs), idsOf(want))
				}
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func idsOf(rs []*types.IssueWithCounts) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Issue.ID)
	}
	return out
}
