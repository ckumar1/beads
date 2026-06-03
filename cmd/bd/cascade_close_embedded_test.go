//go:build cgo

package main

import (
	"os"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestCascadeCloseMoleculeSteps verifies the molecule cascade-DOWN behavior:
// when a molecule (or ephemeral) root closes by any path through `bd close`,
// its open parent-child step-children are auto-closed. This is the inverse of
// autoCloseCompletedMolecule (which cascades UP). Root cause of the recurring
// hq reaper wisp-count alert: patrol molecules whose root was closed by a
// non-last-step path left their step-children open and accumulating (gt-o5z4).
//
// Plain epics must NOT cascade — closing an epic (even forced) leaves its
// children open, matching shouldAutoCloseCompletedRoot's policy.
func TestCascadeCloseMoleculeSteps(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)

	t.Run("molecule_root_close_cascades_to_open_step_children", func(t *testing.T) {
		dir, _, _ := bdInit(t, bd, "--prefix", "cc")
		root := bdCreate(t, bd, dir, "Patrol molecule", "--type", "molecule")
		step1 := bdCreate(t, bd, dir, "Step 1", "--type", "task")
		step2 := bdCreate(t, bd, dir, "Step 2", "--type", "task")
		bdDepAdd(t, bd, dir, step1.ID, root.ID, "--type", "parent-child")
		bdDepAdd(t, bd, dir, step2.ID, root.ID, "--type", "parent-child")

		bdClose(t, bd, dir, root.ID)

		for _, id := range []string{step1.ID, step2.ID} {
			got := bdShow(t, bd, dir, id)
			if got.Status != types.StatusClosed {
				t.Errorf("step %s: expected status %q after molecule root close, got %q",
					id, types.StatusClosed, got.Status)
			}
		}
	})

	t.Run("epic_close_does_not_cascade_to_children", func(t *testing.T) {
		dir, _, _ := bdInit(t, bd, "--prefix", "ce")
		epic := bdCreate(t, bd, dir, "Plain epic", "--type", "epic")
		child1 := bdCreate(t, bd, dir, "Child 1", "--type", "task")
		child2 := bdCreate(t, bd, dir, "Child 2", "--type", "task")
		bdDepAdd(t, bd, dir, child1.ID, epic.ID, "--type", "parent-child")
		bdDepAdd(t, bd, dir, child2.ID, epic.ID, "--type", "parent-child")

		// An epic with open children needs --force to close at all; the cascade
		// must still NOT fire — only molecule/ephemeral roots cascade down.
		bdClose(t, bd, dir, epic.ID, "--force")

		for _, id := range []string{child1.ID, child2.ID} {
			got := bdShow(t, bd, dir, id)
			if got.Status == types.StatusClosed {
				t.Errorf("child %s: epic close must NOT cascade-close children, but it was closed", id)
			}
		}
	})

	t.Run("nested_molecule_cascades_recursively", func(t *testing.T) {
		dir, _, _ := bdInit(t, bd, "--prefix", "cn")
		root := bdCreate(t, bd, dir, "Outer molecule", "--type", "molecule")
		sub := bdCreate(t, bd, dir, "Inner molecule", "--type", "molecule")
		leaf := bdCreate(t, bd, dir, "Leaf step", "--type", "task")
		bdDepAdd(t, bd, dir, sub.ID, root.ID, "--type", "parent-child")
		bdDepAdd(t, bd, dir, leaf.ID, sub.ID, "--type", "parent-child")

		bdClose(t, bd, dir, root.ID)

		for _, id := range []string{sub.ID, leaf.ID} {
			got := bdShow(t, bd, dir, id)
			if got.Status != types.StatusClosed {
				t.Errorf("nested %s: expected status %q after recursive cascade, got %q",
					id, types.StatusClosed, got.Status)
			}
		}
	})
}
