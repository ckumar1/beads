package issueops

import (
	"strings"
	"testing"
)

// The batched mark/unmark templates reach the engine through
// expandBatchTemplate, which fills the batch IN-lists by walking the template
// for "%s" and interleaves the exogeneity ids the template already carries as
// "?" placeholders. Three things are therefore load-bearing and invisible to
// the sqlmock suite next door — its expectations match an unanchored statement
// prefix and never assert bound args, so it stays green against a miscounted
// template: the %s occurrence count, the absence of any other percent sign in
// the finished template (a future LIKE 'x%' or %d in a union leg would shift
// the count or corrupt the statement), and the absence of any "?" the caller
// did not bind. Today a break surfaces only in the container-backed dolt
// suite; these tests pin it at the fast tier (gastownhall/beads#6291).

// batchTemplateOccurrences is the number of batch IN-lists a batched template
// carries: the outer row filter, and one per leg of the scoped
// should-be-blocked union.
//
// It was briefly 8: the parent-child legs' exogeneity set was spliced as the
// query that derives it, and that query carried the batch scope twice more.
// The set is now read once per batch and bound as ids
// (scopedExplainedParentsInTx), which is both 6.9x faster on a 200-id batch
// and why the ? group below exists.
const batchTemplateOccurrences = 6

// explainedFixture is a non-empty exogeneity set of each kind, so the bound-?
// plumbing is exercised rather than degenerating to the empty case.
func explainedFixture() subtreeExplainedParents {
	return subtreeExplainedParents{
		issueParents: []string{"parent-1", "parent-2"},
		wispParents:  []string{"wisp-parent-1"},
	}
}

// batchedTemplates are the batched statements that splice the scoped union:
// the two marks, and the two reads of it the unmarks bind
// (readShouldBeBlockedInTx), which carry no outer row filter and so one
// batch IN-list fewer (batchOccurrences). The unmark statements themselves
// carry no union; TestUnmarkBlockedTemplateBindsTheReadSet pins them.
func batchedTemplates(explained subtreeExplainedParents) map[string]string {
	return map[string]string{
		"markBlockedTemplateForIssues": markBlockedTemplateForIssues(explained),
		"markBlockedTemplateForWisps":  markBlockedTemplateForWisps(explained),
		"unmarkReadForIssues":          shouldBeBlockedIDsUnionScopedPrecomputedSQL("dependencies", batchScopeSQL, explained),
		"unmarkReadForWisps":           shouldBeBlockedIDsUnionScopedPrecomputedSQL("wisp_dependencies", batchScopeSQL, explained),
	}
}

func batchOccurrences(name string) int {
	if strings.HasPrefix(name, "unmarkRead") {
		return batchTemplateOccurrences - 1
	}
	return batchTemplateOccurrences
}

func TestBatchedTemplatePercentBudget(t *testing.T) {
	for name, tmpl := range batchedTemplates(explainedFixture()) {
		t.Run(name, func(t *testing.T) {
			want := batchOccurrences(name)
			if got := strings.Count(tmpl, "%s"); got != want {
				t.Errorf("%%s count = %d, want %d — expandBatchTemplate binds one batch id group per occurrence, so a changed count must be a deliberate edit here", got, want)
			}
			// The stray-percent guard: every percent sign in the finished
			// template must belong to one of the %s verbs counted above.
			if got := strings.Count(tmpl, "%"); got != want {
				t.Errorf("total %% count = %d, want %d — a percent sign outside a %%s (LIKE 'x%%', %%d, %%%%) miscounts the template or corrupts the statement", got, want)
			}
		})
	}
}

// TestBatchedTemplateQuestionMarksAreTheExogeneitySet pins the other half of
// expandBatchTemplate's contract: the ONLY ? a template carries are the two
// parent-child legs' bound exogeneity ids, so an empty set leaves none.
func TestBatchedTemplateQuestionMarksAreTheExogeneitySet(t *testing.T) {
	explained := explainedFixture()
	want := len(explained.issueParents) + len(explained.wispParents)
	for name, tmpl := range batchedTemplates(explained) {
		t.Run(name, func(t *testing.T) {
			if got := strings.Count(tmpl, "?"); got != want {
				t.Errorf("? count = %d, want %d (the bound exogeneity ids)", got, want)
			}
		})
	}
	for name, tmpl := range batchedTemplates(subtreeExplainedParents{}) {
		t.Run(name+"/empty", func(t *testing.T) {
			if got := strings.Count(tmpl, "?"); got != 0 {
				t.Errorf("? count = %d over an empty exogeneity set, want 0", got)
			}
		})
	}
}

func TestWaitsForGateBlockedSQLCarriesNoPercentOrPlaceholder(t *testing.T) {
	// The gate is spliced into every template as a resolved constant, so any
	// percent sign or placeholder it grew would land in the counted text above.
	if got := strings.Count(waitsForGateBlockedSQL, "%"); got != 0 {
		t.Errorf("waitsForGateBlockedSQL %% count = %d, want 0", got)
	}
	if got := strings.Count(waitsForGateBlockedSQL, "?"); got != 0 {
		t.Errorf("waitsForGateBlockedSQL ? count = %d, want 0", got)
	}
}

func TestExpandBatchTemplateFillsEveryOccurrence(t *testing.T) {
	placeholders, args := buildSQLInClause([]string{"issue-1", "issue-2"})
	explained := explainedFixture()
	bound := explained.args()

	for name, tmpl := range batchedTemplates(explained) {
		t.Run(name, func(t *testing.T) {
			occurrences := batchOccurrences(name)
			wantArgs := occurrences*len(args) + len(bound)
			stmt, stmtArgs, err := expandBatchTemplate(tmpl, placeholders, args, bound)
			if err != nil {
				t.Fatalf("expandBatchTemplate: %v", err)
			}

			if strings.ContainsAny(stmt, "%") {
				t.Errorf("expanded statement still contains %%: %s", stmt)
			}
			if got := strings.Count(stmt, "?"); got != wantArgs {
				t.Errorf("placeholder count = %d, want %d", got, wantArgs)
			}
			if got := len(stmtArgs); got != wantArgs {
				t.Fatalf("arg count = %d, want %d — placeholders and args must stay in lockstep", got, wantArgs)
			}
			// TEXT ORDER: the batch group for the outer filter (marks only)
			// and legs 1-2, then leg 3's batch group followed by the issue
			// parents, then leg 4's batch group followed by the wisp parents,
			// then leg 5's.
			var want []interface{}
			for k := 0; k < occurrences; k++ {
				want = append(want, args...)
				if k == occurrences-3 {
					want = append(want, "parent-1", "parent-2")
				}
				if k == occurrences-2 {
					want = append(want, "wisp-parent-1")
				}
			}
			for i, arg := range stmtArgs {
				if arg != want[i] {
					t.Errorf("arg %d = %v, want %v", i, arg, want[i])
				}
			}
		})
	}
}

// TestUnmarkBlockedTemplateBindsTheReadSet pins the unmark's shape: no
// union, no subquery — the batch once, then the ids readShouldBeBlockedInTx
// returned, in that text order — and no `NOT IN ()` over an empty read.
func TestUnmarkBlockedTemplateBindsTheReadSet(t *testing.T) {
	placeholders, args := buildSQLInClause([]string{"issue-1", "issue-2"})
	for name, tmpl := range map[string]string{
		"issues": unmarkBlockedTemplateForIssues(1),
		"wisps":  unmarkBlockedTemplateForWisps(1),
	} {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(tmpl, "SELECT") || strings.Contains(tmpl, "UNION") {
				t.Errorf("unmark template carries a subquery:\n%s", tmpl)
			}
			stmt, stmtArgs, err := expandBatchTemplate(tmpl, placeholders, args, []interface{}{"issue-2"})
			if err != nil {
				t.Fatalf("expandBatchTemplate: %v", err)
			}
			if !strings.Contains(stmt, ".id NOT IN (?) )") {
				t.Errorf("unmark does not exclude the read set:\n%s", stmt)
			}
			want := []interface{}{"issue-1", "issue-2", "issue-2"}
			if len(stmtArgs) != len(want) {
				t.Fatalf("args = %v, want %v", stmtArgs, want)
			}
			for i := range want {
				if stmtArgs[i] != want[i] {
					t.Errorf("arg %d = %v, want %v", i, stmtArgs[i], want[i])
				}
			}
		})
	}
	empty := unmarkBlockedTemplateForIssues(0)
	if strings.Contains(empty, "NOT IN") || strings.Contains(empty, "status") || strings.Contains(empty, "?") {
		t.Errorf("unmark over an empty read should clear every flagged batch row:\n%s", empty)
	}
}

// TestExpandBatchTemplateRefusesABoundMismatch is the guard that keeps a
// miscounted bound group from reaching the engine as an opaque argument-count
// error: the template's ? placeholders and the bound ids must agree exactly.
func TestExpandBatchTemplateRefusesABoundMismatch(t *testing.T) {
	placeholders, args := buildSQLInClause([]string{"issue-1"})
	tmpl := markBlockedTemplateForIssues(subtreeExplainedParents{issueParents: []string{"parent-1"}})

	if _, _, err := expandBatchTemplate(tmpl, placeholders, args, nil); err == nil {
		t.Error("want an error when the template carries a ? the caller did not bind")
	}
	if _, _, err := expandBatchTemplate(
		tmpl, placeholders, args, []interface{}{"parent-1", "parent-2"}); err == nil {
		t.Error("want an error when the caller binds more ids than the template carries")
	}
}

func TestExpandBatchTemplateSingleOccurrenceDegrades(t *testing.T) {
	// A template with one %s and no bound ids is the plain Sprintf it always
	// was: the args pass through untouched rather than being repeated.
	placeholders, args := buildSQLInClause([]string{"issue-1", "issue-2"})
	stmt, stmtArgs, err := expandBatchTemplate(
		"SELECT id FROM issues WHERE id IN (%s)", placeholders, args, nil)
	if err != nil {
		t.Fatalf("expandBatchTemplate: %v", err)
	}

	if want := "SELECT id FROM issues WHERE id IN (?,?)"; stmt != want {
		t.Errorf("stmt = %q, want %q", stmt, want)
	}
	if len(stmtArgs) != len(args) {
		t.Errorf("arg count = %d, want %d", len(stmtArgs), len(args))
	}
}

// TestBatchedTemplatesPinLookupJoins pins the join hints on the scoped
// should-be-blocked union's four joined legs: without them the Dolt
// sql-server planner intermittently drives the issues legs from an index
// scan of every open issue, 6-10 s per statement over an import's
// uncommitted working set (see shouldBeBlockedIDsUnionScopedPrecomputedSQL).
// The unscoped union (full repair, doctor count) keeps the planner's choice.
func TestBatchedTemplatesPinLookupJoins(t *testing.T) {
	for name, tmpl := range batchedTemplates(explainedFixture()) {
		for _, hint := range []string{"JOIN_ORDER(d, t) LOOKUP_JOIN(d, t)", "JOIN_ORDER(d, p) LOOKUP_JOIN(d, p)"} {
			if got := strings.Count(tmpl, hint); got != 2 {
				t.Errorf("%s carries %q %d times, want 2 (one per joined issues/wisps leg)", name, hint, got)
			}
		}
	}
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		union := shouldBeBlockedIDsUnionPrecomputedSQL(depTable, explainedFixture())
		if strings.Contains(union, "LOOKUP_JOIN(d, t)") || strings.Contains(union, "LOOKUP_JOIN(d, p)") {
			t.Errorf("unscoped union over %s should not pin its legs' joins", depTable)
		}
	}
}

// TestWaitsForGateSplitsSpawnerColumns pins the waits-for gate's per-column
// EXISTS split: one OR across both spawner columns inside an EXISTS has no
// index to use and scans the edge table per evaluation (see
// waitsForGateBlockedSQL). Two tables x two columns x {open, closed} children.
func TestWaitsForGateSplitsSpawnerColumns(t *testing.T) {
	if got := strings.Count(waitsForGateBlockedSQL, "JOIN_ORDER(cd, child) LOOKUP_JOIN(cd, child)"); got != 8 {
		t.Errorf("gate pins %d child lookups, want 8", got)
	}
	if strings.Contains(waitsForGateBlockedSQL, "OR (d.depends_on_wisp_id IS NOT NULL") {
		t.Errorf("gate still ORs the spawner columns inside one EXISTS")
	}
	for _, col := range []string{"depends_on_issue_id", "depends_on_wisp_id"} {
		if got := strings.Count(waitsForGateBlockedSQL, "cd."+col+" = d."+col); got != 4 {
			t.Errorf("gate matches cd.%s in %d EXISTS, want 4", col, got)
		}
	}
}
