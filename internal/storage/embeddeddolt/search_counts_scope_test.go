//go:build cgo

package embeddeddolt_test

import (
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// TestSearchIssuesWithCountsScoped is the correctness guard for the scoped
// aggregation rewrite of runSearchQueryInTx (gt-1qjp). The aggregations
// (dep_count, rdep_count, comment_count, parent, deps_json) were previously
// computed via UNSCOPED LEFT JOIN derived tables that aggregated the ENTIRE
// dependencies/comments/labels tables on every query. The rewrite bounds each
// aggregation to the CTE-filtered issue set. This test asserts that the
// observable per-issue counts/parent/labels/deps are UNCHANGED across the
// LIMIT, no-LIMIT, status-filter, wisps-path, and skipLabels code paths.
//
// The key scoping correctness check is the LIMIT case: with a LIMIT smaller
// than the issue count, the aggregations must still report the FULL count for
// each returned issue (scoping to the returned set must not under-count edges
// that originate from returned issues), and must select the correct rows.
func TestSearchIssuesWithCountsScoped(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	te := newTestEnv(t, "sc")
	ctx := t.Context()

	// Fixture (distinct priorities give deterministic ORDER BY priority ASC):
	//   sc-a (p0)  parent of sc-b, sc-c; has 2 labels; 1 comment
	//   sc-b (p1)  child of sc-a; blocks sc-d; has 1 label
	//   sc-c (p2)  child of sc-a; blocks sc-d
	//   sc-d (p3)  blocked by sc-b and sc-c (rdep_count via reverse blockers)
	//   sc-e (p4)  isolated, no edges, no labels, no comments
	issues := []*types.Issue{
		{ID: "sc-a", Title: "alpha", Status: types.StatusOpen, Priority: 0, IssueType: types.TypeTask},
		{ID: "sc-b", Title: "bravo", Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask},
		{ID: "sc-c", Title: "charlie", Status: types.StatusInProgress, Priority: 2, IssueType: types.TypeTask},
		{ID: "sc-d", Title: "delta", Status: types.StatusOpen, Priority: 3, IssueType: types.TypeTask},
		{ID: "sc-e", Title: "echo", Status: types.StatusOpen, Priority: 4, IssueType: types.TypeTask},
	}
	for _, issue := range issues {
		if err := te.store.CreateIssue(ctx, issue, "tester"); err != nil {
			t.Fatalf("CreateIssue %s: %v", issue.ID, err)
		}
	}

	deps := []*types.Dependency{
		{IssueID: "sc-b", DependsOnID: "sc-a", Type: types.DepParentChild},
		{IssueID: "sc-c", DependsOnID: "sc-a", Type: types.DepParentChild},
		{IssueID: "sc-b", DependsOnID: "sc-d", Type: types.DepBlocks},
		{IssueID: "sc-c", DependsOnID: "sc-d", Type: types.DepBlocks},
	}
	for _, dep := range deps {
		if err := te.store.AddDependency(ctx, dep, "tester"); err != nil {
			t.Fatalf("AddDependency %s->%s: %v", dep.IssueID, dep.DependsOnID, err)
		}
	}

	for _, l := range []struct{ id, label string }{
		{"sc-a", "area:core"},
		{"sc-a", "kind:epic"},
		{"sc-b", "area:core"},
	} {
		if err := te.store.AddLabel(ctx, l.id, l.label, "tester"); err != nil {
			t.Fatalf("AddLabel %s %s: %v", l.id, l.label, err)
		}
	}

	// ImportIssueComment writes a real row to the comments table (which the
	// comment_count aggregation reads); AddComment only writes an events row.
	if _, err := te.store.ImportIssueComment(ctx, "sc-a", "tester", "first comment", time.Now().UTC()); err != nil {
		t.Fatalf("ImportIssueComment sc-a: %v", err)
	}

	if err := te.store.Commit(ctx, "fixture"); err != nil {
		t.Fatalf("Commit fixture: %v", err)
	}

	// want describes the expected aggregation results per issue.
	type want struct {
		depCount     int
		rdepCount    int
		commentCount int
		parent       string // "" means no parent
		labels       []string
		depIDs       []string // depends_on ids in deps_json (sorted)
	}
	wantAll := map[string]want{
		// sc-a: blocks=0; rdep: sc-a is the depends_on of 2 parent-child edges
		//   (sc-b, sc-c), but reverse-blocker counts only type='blocks', so 0.
		//   comment_count=1; parent none; labels area:core,kind:epic;
		//   deps_json holds NO outgoing edges (sc-a is never the issue_id).
		"sc-a": {depCount: 0, rdepCount: 0, commentCount: 1, parent: "", labels: []string{"area:core", "kind:epic"}, depIDs: nil},
		// sc-b: 1 blocks edge (->sc-d); rdep 0; comment 0; parent sc-a;
		//   1 label; deps_json: parent-child->sc-a + blocks->sc-d.
		"sc-b": {depCount: 1, rdepCount: 0, commentCount: 0, parent: "sc-a", labels: []string{"area:core"}, depIDs: []string{"sc-a", "sc-d"}},
		// sc-c: 1 blocks edge (->sc-d); parent sc-a; no labels.
		"sc-c": {depCount: 1, rdepCount: 0, commentCount: 0, parent: "sc-a", labels: nil, depIDs: []string{"sc-a", "sc-d"}},
		// sc-d: no outgoing; reverse blockers = 2 (sc-b, sc-c block it).
		"sc-d": {depCount: 0, rdepCount: 2, commentCount: 0, parent: "", labels: nil, depIDs: nil},
		// sc-e: isolated.
		"sc-e": {depCount: 0, rdepCount: 0, commentCount: 0, parent: "", labels: nil, depIDs: nil},
	}

	assertMatches := func(t *testing.T, iwc *types.IssueWithCounts, w want, checkLabels bool) {
		t.Helper()
		id := iwc.Issue.ID
		if iwc.DependencyCount != w.depCount {
			t.Errorf("%s DependencyCount = %d, want %d", id, iwc.DependencyCount, w.depCount)
		}
		if iwc.DependentCount != w.rdepCount {
			t.Errorf("%s DependentCount = %d, want %d", id, iwc.DependentCount, w.rdepCount)
		}
		if iwc.CommentCount != w.commentCount {
			t.Errorf("%s CommentCount = %d, want %d", id, iwc.CommentCount, w.commentCount)
		}
		gotParent := ""
		if iwc.Parent != nil {
			gotParent = *iwc.Parent
		}
		if gotParent != w.parent {
			t.Errorf("%s Parent = %q, want %q", id, gotParent, w.parent)
		}
		if checkLabels {
			if !equalStringSets(iwc.Issue.Labels, w.labels) {
				t.Errorf("%s Labels = %v, want %v", id, iwc.Issue.Labels, w.labels)
			}
		} else {
			if len(iwc.Issue.Labels) != 0 {
				t.Errorf("%s skipLabels: Labels = %v, want none", id, iwc.Issue.Labels)
			}
		}
		gotDepIDs := dependsOnIDs(iwc.Issue.Dependencies)
		if !equalStringSets(gotDepIDs, w.depIDs) {
			t.Errorf("%s deps_json depends_on = %v, want %v", id, gotDepIDs, w.depIDs)
		}
	}

	indexByID := func(items []*types.IssueWithCounts) map[string]*types.IssueWithCounts {
		m := make(map[string]*types.IssueWithCounts, len(items))
		for _, it := range items {
			m[it.Issue.ID] = it
		}
		return m
	}

	t.Run("no limit returns all with correct scoped counts", func(t *testing.T) {
		got, err := te.store.SearchIssuesWithCounts(ctx, "", types.IssueFilter{})
		if err != nil {
			t.Fatalf("SearchIssuesWithCounts: %v", err)
		}
		if len(got) != len(issues) {
			t.Fatalf("got %d issues, want %d", len(got), len(issues))
		}
		idx := indexByID(got)
		for id, w := range wantAll {
			iwc, ok := idx[id]
			if !ok {
				t.Fatalf("missing %s in results", id)
			}
			assertMatches(t, iwc, w, true)
		}
	})

	t.Run("limit smaller than count selects correct rows and full counts", func(t *testing.T) {
		// ORDER BY priority ASC, created_at DESC, id ASC => sc-a(0), sc-b(1), sc-c(2).
		got, err := te.store.SearchIssuesWithCounts(ctx, "", types.IssueFilter{Limit: 3})
		if err != nil {
			t.Fatalf("SearchIssuesWithCounts: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d issues, want 3", len(got))
		}
		gotOrder := []string{got[0].Issue.ID, got[1].Issue.ID, got[2].Issue.ID}
		wantOrder := []string{"sc-a", "sc-b", "sc-c"}
		for i := range wantOrder {
			if gotOrder[i] != wantOrder[i] {
				t.Fatalf("limited order = %v, want %v", gotOrder, wantOrder)
			}
		}
		idx := indexByID(got)
		// Critical scoping assertion: sc-b's blocks edge targets sc-d, which is
		// NOT in the limited set, yet sc-b's dep_count must still be 1. The
		// scoping bounds aggregation by the AGGREGATING side's issue_id (in the
		// returned set), not by the edge's target.
		for _, id := range wantOrder {
			assertMatches(t, idx[id], wantAll[id], true)
		}
	})

	t.Run("status filter scopes aggregations to filtered set", func(t *testing.T) {
		open := types.StatusOpen
		got, err := te.store.SearchIssuesWithCounts(ctx, "", types.IssueFilter{Status: &open})
		if err != nil {
			t.Fatalf("SearchIssuesWithCounts: %v", err)
		}
		// sc-c is in_progress; the rest are open.
		idx := indexByID(got)
		if _, present := idx["sc-c"]; present {
			t.Fatalf("status=open should exclude sc-c (in_progress)")
		}
		if len(got) != 4 {
			t.Fatalf("status=open got %d issues, want 4", len(got))
		}
		for _, id := range []string{"sc-a", "sc-b", "sc-d", "sc-e"} {
			iwc, ok := idx[id]
			if !ok {
				t.Fatalf("missing %s under status=open", id)
			}
			// sc-d's rdep_count must still be 2 even though one of its
			// blockers (sc-c) is filtered out of the returned set — reverse
			// blockers are scoped by dep_id (the blocked issue, sc-d, which IS
			// in the set), not by the blocker's membership.
			assertMatches(t, iwc, wantAll[id], true)
		}
	})

	t.Run("skipLabels suppresses label hydration", func(t *testing.T) {
		got, err := te.store.SearchIssuesWithCounts(ctx, "", types.IssueFilter{SkipLabels: true})
		if err != nil {
			t.Fatalf("SearchIssuesWithCounts: %v", err)
		}
		if len(got) != len(issues) {
			t.Fatalf("got %d issues, want %d", len(got), len(issues))
		}
		idx := indexByID(got)
		for id, w := range wantAll {
			assertMatches(t, idx[id], w, false)
		}
	})
}

// TestSearchIssuesWithCountsScopedWispsPath exercises the wisps FilterTables
// branch of runSearchQueryInTx (Ephemeral filter routes to the wisps/
// wisp_dependencies/wisp_comments/wisp_labels tables).
func TestSearchIssuesWithCountsScopedWispsPath(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	te := newTestEnv(t, "sw")
	ctx := t.Context()

	// Two wisps; sw-x blocks sw-y. sw-x has a label and a comment.
	wisps := []*types.Issue{
		{ID: "sw-x", Title: "wisp x", Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask, Ephemeral: true},
		{ID: "sw-y", Title: "wisp y", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: true},
	}
	for _, w := range wisps {
		if err := te.store.CreateIssue(ctx, w, "tester"); err != nil {
			t.Fatalf("CreateIssue %s: %v", w.ID, err)
		}
	}
	if err := te.store.AddDependency(ctx, &types.Dependency{IssueID: "sw-x", DependsOnID: "sw-y", Type: types.DepBlocks}, "tester"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	if err := te.store.AddLabel(ctx, "sw-x", "ephemeral:true", "tester"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	// ImportIssueComment routes ephemeral issues to wisp_comments (the table the
	// wisps-path comment_count aggregation reads). Ephemeral writes are not
	// version-controlled, so no Commit is issued.
	if _, err := te.store.ImportIssueComment(ctx, "sw-x", "tester", "wisp comment", time.Now().UTC()); err != nil {
		t.Fatalf("ImportIssueComment: %v", err)
	}

	ephemeral := true
	got, err := te.store.SearchIssuesWithCounts(ctx, "", types.IssueFilter{Ephemeral: &ephemeral})
	if err != nil {
		t.Fatalf("SearchIssuesWithCounts (wisps): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("wisps path got %d, want 2", len(got))
	}
	idx := make(map[string]*types.IssueWithCounts, len(got))
	for _, it := range got {
		idx[it.Issue.ID] = it
	}

	x := idx["sw-x"]
	if x == nil {
		t.Fatal("missing sw-x")
	}
	if x.DependencyCount != 1 {
		t.Errorf("sw-x DependencyCount = %d, want 1", x.DependencyCount)
	}
	if x.CommentCount != 1 {
		t.Errorf("sw-x CommentCount = %d, want 1", x.CommentCount)
	}
	if !equalStringSets(x.Issue.Labels, []string{"ephemeral:true"}) {
		t.Errorf("sw-x Labels = %v, want [ephemeral:true]", x.Issue.Labels)
	}
	if !equalStringSets(dependsOnIDs(x.Issue.Dependencies), []string{"sw-y"}) {
		t.Errorf("sw-x deps = %v, want [sw-y]", dependsOnIDs(x.Issue.Dependencies))
	}

	y := idx["sw-y"]
	if y == nil {
		t.Fatal("missing sw-y")
	}
	if y.DependentCount != 1 {
		t.Errorf("sw-y DependentCount = %d, want 1 (blocked by sw-x)", y.DependentCount)
	}
}

func dependsOnIDs(deps []*types.Dependency) []string {
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		out = append(out, d.DependsOnID)
	}
	return out
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
		if counts[s] < 0 {
			return false
		}
	}
	for _, c := range counts {
		if c != 0 {
			return false
		}
	}
	return true
}
