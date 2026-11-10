//go:build cgo

package embeddeddolt_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/types"
)

// BenchmarkCloseBlockerUnmarksDependents measures Store.CloseIssue on a row
// that many open rows are blocked by, against a real embedded Dolt store. The
// close recomputes is_blocked for every row it unblocks, and that recompute
// clears their flags through the batched mark/unmark statements, one chunk of
// up to 200 ids at a time: the unmark is the statement this benchmark is for.
//
//   - blocks_fanin: BEADS_BENCH_DEPENDENTS rows (default 300, so two chunks)
//     each block on one blocker; the timed close is the blocker's.
//   - parent_child: a parent blocked by an outside blocker, with that many
//     children dark under it; the timed close is the outside blocker's, so
//     the children clear through the parent-child legs as the parent does.
//
// Each iteration closes the blocker (timed), then, untimed, checks that no
// row is left blocked, reopens the blocker and checks that every dependent is
// blocked again, so every iteration starts from the same state and an
// iteration that cleared nothing fails. Set BEADS_TEST_EMBEDDED_DOLT=1 to run.
func BenchmarkCloseBlockerUnmarksDependents(b *testing.B) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		b.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt benchmarks")
	}
	dependents := 300
	if raw := os.Getenv("BEADS_BENCH_DEPENDENTS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			b.Fatalf("BEADS_BENCH_DEPENDENTS=%q: want a positive integer", raw)
		}
		dependents = parsed
	}

	for _, arm := range []struct {
		name string
		// depType is each dependent's edge to target; with a parent, the
		// parent blocks on the blocker and the dependents hang under it.
		depType   types.DependencyType
		hasParent bool
	}{
		{"blocks_fanin", types.DepBlocks, false},
		{"parent_child", types.DepParentChild, true},
	} {
		b.Run(arm.name, func(b *testing.B) {
			ctx := b.Context()
			store, err := embeddeddolt.Open(ctx, filepath.Join(b.TempDir(), ".beads"), "bench", "main")
			if err != nil {
				b.Fatalf("open embedded Dolt store: %v", err)
			}
			b.Cleanup(func() {
				if err := store.Close(); err != nil {
					b.Errorf("close embedded Dolt store: %v", err)
				}
			})
			if err := store.SetConfig(ctx, "issue_prefix", "bench"); err != nil {
				b.Fatalf("set issue_prefix: %v", err)
			}

			newIssue := func(id string, deps ...*types.Dependency) *types.Issue {
				return &types.Issue{ID: id, Title: id, Status: types.StatusOpen, Priority: 2,
					IssueType: types.TypeTask, Dependencies: deps}
			}
			const blocker = "bench-blocker"
			seed := []*types.Issue{newIssue(blocker)}
			target, wantBlocked := blocker, dependents
			if arm.hasParent {
				target, wantBlocked = "bench-parent", dependents+1
				seed = append(seed, newIssue(target, &types.Dependency{DependsOnID: blocker, Type: types.DepBlocks}))
			}
			if err := store.CreateIssues(ctx, seed, "bench"); err != nil {
				b.Fatalf("create blocker: %v", err)
			}
			batch := make([]*types.Issue, 0, dependents)
			for i := range dependents {
				batch = append(batch, newIssue(fmt.Sprintf("bench-d%05d", i),
					&types.Dependency{DependsOnID: target, Type: arm.depType}))
			}
			if err := store.CreateIssues(ctx, batch, "bench"); err != nil {
				b.Fatalf("create dependents: %v", err)
			}

			blocked := true
			assertBlocked := func(when string, want int) {
				b.Helper()
				rows, err := store.SearchIssues(ctx, "", types.IssueFilter{IsBlocked: &blocked})
				if err != nil {
					b.Fatalf("%s: SearchIssues: %v", when, err)
				}
				if len(rows) != want {
					b.Fatalf("%s: %d rows blocked, want %d", when, len(rows), want)
				}
			}
			assertBlocked("after seeding", wantBlocked)

			for b.Loop() {
				if err := store.CloseIssue(ctx, blocker, "bench", "bench", ""); err != nil {
					b.Fatalf("close blocker: %v", err)
				}
				b.StopTimer()
				assertBlocked("after close", 0)
				if err := store.ReopenIssue(ctx, blocker, "bench", "bench"); err != nil {
					b.Fatalf("reopen blocker: %v", err)
				}
				assertBlocked("after reopen", wantBlocked)
				b.StartTimer()
			}
		})
	}
}
