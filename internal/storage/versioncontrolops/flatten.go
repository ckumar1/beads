package versioncontrolops

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage/schema"
)

// Flatten squashes all Dolt commit history into a single commit using
// the Tim Sehn recipe:
//  1. Create a temp branch from current state
//  2. Checkout temp branch
//  3. Soft-reset to the initial (oldest) commit, collapsing all history
//  4. Stage all + commit as a single snapshot
//  5. Checkout main
//  6. Hard-reset main to the flattened branch
//  7. Delete temp branch
//
// Callers should run PruneRemoteRefs and then DoltGC afterward to reclaim disk
// space from orphaned history — remote-tracking refs still anchor the
// pre-flatten chain, and GC alone reclaims nothing while they exist (bd-agctw).
//
// conn must be a single database connection (not a pooled *sql.DB) since the
// stored procedures rely on session-scoped state (current branch, working set).
func Flatten(ctx context.Context, conn DBConn) error {
	// Find the initial commit hash (oldest ancestor).
	var initialHash string
	if err := conn.QueryRowContext(ctx,
		"SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1",
	).Scan(&initialHash); err != nil {
		return fmt.Errorf("find initial commit: %w", err)
	}

	// Count commits to check if flatten is needed.
	var commitCount int
	if err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM dolt_log",
	).Scan(&commitCount); err != nil {
		return fmt.Errorf("count commits: %w", err)
	}
	if commitCount <= 1 {
		return nil // already flat
	}

	execSQL := func(name, query string, args ...interface{}) error {
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("flatten step %q: %w", name, err)
		}
		return nil
	}

	steps := []struct {
		name  string
		query string
		args  []interface{}
	}{
		{"create temp branch", "CALL DOLT_BRANCH('flatten-tmp')", nil},
		{"checkout temp branch", "CALL DOLT_CHECKOUT('flatten-tmp')", nil},
		{"soft reset to initial", "CALL DOLT_RESET('--soft', ?)", []interface{}{initialHash}},
		{"commit flattened snapshot", "CALL DOLT_COMMIT('-Am', 'flatten: squash all history into single commit')", nil},
		{"checkout main", "CALL DOLT_CHECKOUT('main')", nil},
	}

	for _, s := range steps {
		if err := execSQL(s.name, s.query, s.args...); err != nil {
			return err
		}
	}

	// bd-7bpkd / ga-28co77: the hard reset drops every clone-local FK; the
	// helper re-links the ones it dropped, on this same session. A re-link
	// failure still means main was reset, so the temp branch is deleted
	// before the failure is returned — a leftover flatten-tmp would block
	// every later flatten at "create temp branch".
	resetErr := resetHardPreservingCloneLocalFKs(ctx, conn, "flatten-tmp")
	var relinkErr *schema.CloneLocalFKRelinkError
	if resetErr != nil && !errors.As(resetErr, &relinkErr) {
		return fmt.Errorf("flatten step %q: %w", "reset main to flattened", resetErr)
	}
	if err := execSQL("delete temp branch", "CALL DOLT_BRANCH('-D', 'flatten-tmp')"); err != nil {
		return errors.Join(err, resetErr)
	}
	if resetErr != nil {
		return fmt.Errorf("flatten step %q: %w", "reset main to flattened", resetErr)
	}

	return nil
}

// FlattenDryRun returns the commit count and initial hash without modifying anything.
func FlattenDryRun(ctx context.Context, conn DBConn) (commitCount int, initialHash string, err error) {
	if err = conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM dolt_log",
	).Scan(&commitCount); err != nil {
		err = fmt.Errorf("count commits: %w", err)
		return
	}
	if err = conn.QueryRowContext(ctx,
		"SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1",
	).Scan(&initialHash); err != nil {
		err = fmt.Errorf("find initial commit: %w", err)
		return
	}
	return
}
