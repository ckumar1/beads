package versioncontrolops

import (
	"context"
	"fmt"
	"os"

	"github.com/steveyegge/beads/internal/storage/schema"
)

// resetHardPreservingCloneLocalFKs runs CALL DOLT_RESET('--hard'[, target]) on
// conn through schema.ResetHardPreservingCloneLocalFKs, which re-links every
// clone-local FK the reset drops (bd-7bpkd, ga-28co77). conn must be the same
// session the caller's preceding checkout ran on.
//
// FKs that were already severed before the reset are left untouched and named
// on stderr, like this package's other operator notices, so the fault reaches
// the operator running the command; the heal for those stays
// `bd doctor --fix`. A re-link failure is returned as a
// *schema.CloneLocalFKRelinkError (the reset itself succeeded).
func resetHardPreservingCloneLocalFKs(ctx context.Context, conn DBConn, target string) error {
	result, err := schema.ResetHardPreservingCloneLocalFKs(ctx, conn, target)
	if w := result.Warning(); w != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}
	return err
}
