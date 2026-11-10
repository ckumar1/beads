//go:build cgo

package embeddeddolt_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/types"
)

// BenchmarkSearchIssuesWithCounts measures Store.SearchIssuesWithCounts, the
// read behind `bd list` and `bd search` with counts, against a real embedded
// Dolt store whose side tables are large next to the page a search returns.
//
// The counts query hydrates each row with its labels, blocker counts, comment
// count, parent and dependency JSON. The question this benchmark answers is
// how the cost of that hydration scales with the size of the side tables when
// the page itself is small:
//
//   - narrow_filter_limited: an assignee and status filter that matches one
//     issue in narrowEvery (10 at the default size), with a LIMIT of 20.
//   - unfiltered_limited: no filter, the first page of 50 in default order.
//
// Workload: BEADS_BENCH_ISSUES issues (default 15000), each with three labels;
// every fourth issue is a hub that three neighbours block on (about 3/4 of the
// issue count in dependency rows), and every fifth issue carries a comment.
// Seeding runs once, in batches, before either arm is timed. Set
// BEADS_TEST_EMBEDDED_DOLT=1 to run.
func BenchmarkSearchIssuesWithCounts(b *testing.B) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		b.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt benchmarks")
	}

	issueCount := 15000
	if raw := os.Getenv("BEADS_BENCH_ISSUES"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 400 {
			b.Fatalf("BEADS_BENCH_ISSUES=%q: want an integer of at least 400", raw)
		}
		issueCount = parsed
	}
	const (
		batchSize     = 1000 // a multiple of 4, so every hub lands in its neighbours' batch
		narrowEvery   = 1500 // one issue in this many belongs to the narrow filter's owner
		narrowLimit   = 20
		unfilteredCap = 50
	)
	narrowMatches := (issueCount + narrowEvery - 1) / narrowEvery

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

	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for start := 0; start < issueCount; start += batchSize {
		batch := make([]*types.Issue, 0, batchSize)
		for i := start; i < min(start+batchSize, issueCount); i++ {
			issue := &types.Issue{
				ID:        fmt.Sprintf("bench-%05d", i),
				Title:     fmt.Sprintf("Bench issue %d", i),
				Status:    types.StatusOpen,
				Priority:  i % 4,
				IssueType: types.TypeTask,
				CreatedAt: created.Add(time.Duration(i) * time.Second),
				Labels:    []string{fmt.Sprintf("area-%d", i%20), fmt.Sprintf("tier-%d", i%3), fmt.Sprintf("team-%d", i%50)},
			}
			if i%narrowEvery == 0 {
				issue.Assignee = "narrow-owner"
			}
			if hub := i - i%4; hub != i {
				issue.Dependencies = []*types.Dependency{{DependsOnID: fmt.Sprintf("bench-%05d", hub), Type: types.DepBlocks}}
			}
			if i%5 == 0 {
				issue.Comments = []*types.Comment{{Author: "bench", Text: fmt.Sprintf("note %d", i), CreatedAt: issue.CreatedAt}}
			}
			batch = append(batch, issue)
		}
		if err := store.CreateIssues(ctx, batch, "bench"); err != nil {
			b.Fatalf("CreateIssues batch at %d: %v", start, err)
		}
	}
	if err := store.Commit(ctx, "bench seed"); err != nil {
		b.Fatalf("commit seed: %v", err)
	}

	owner := "narrow-owner"
	for _, arm := range []struct {
		name   string
		filter types.IssueFilter
		want   int
	}{
		{"narrow_filter_limited", types.IssueFilter{Assignee: &owner, Statuses: []types.Status{types.StatusOpen}, Limit: narrowLimit}, min(narrowMatches, narrowLimit)},
		{"unfiltered_limited", types.IssueFilter{Limit: unfilteredCap}, unfilteredCap},
	} {
		b.Run(arm.name, func(b *testing.B) {
			for b.Loop() {
				rows, err := store.SearchIssuesWithCounts(ctx, "", arm.filter)
				if err != nil {
					b.Fatalf("SearchIssuesWithCounts: %v", err)
				}
				if len(rows) != arm.want {
					b.Fatalf("SearchIssuesWithCounts returned %d rows, want %d", len(rows), arm.want)
				}
			}
		})
	}
}
