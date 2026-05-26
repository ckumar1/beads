package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/storage"
)

// autopushWorkerCmd is a hidden subcommand spawned in a detached subprocess by
// maybeAutoPush. It performs the actual `dolt push` outside the lifetime of
// the bd parent process, so the push isn't bounded by either:
//
//   - the 30s inline autopush timeout (chosen to fit under gt's 60s
//     bd-subprocess kill window), or
//   - gt's SIGKILL of bd subprocesses that don't return in 60s.
//
// Without this, a clone that has accumulated more than ~30s worth of unpushed
// chunks can never auto-recover. The worker uses cliExecTimeout (5min) which
// is the right bound for a "this might be a large catch-up" push. (hq-8nkpj4)
var autopushWorkerCmd = &cobra.Command{
	Use:    "_autopush-worker",
	Short:  "[internal] Detached autopush worker — do not invoke directly",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAutopushWorker(cmd.Context())
	},
}

func runAutopushWorker(ctx context.Context) error {
	st := getStore()
	if st == nil {
		return fmt.Errorf("autopush worker: store unavailable")
	}
	if lm, ok := storage.UnwrapStore(st).(storage.LifecycleManager); ok && lm.IsClosed() {
		return nil
	}

	// 5-minute cap: long enough for sizable catch-ups, short enough that a
	// genuinely hung remote doesn't tie up resources indefinitely.
	//
	// The parent pre-stamps LastPush before forking, so the 5-minute debounce
	// suppresses subsequent triggers once the stamp lands. This covers the
	// common case (writes spaced more than a few ms apart). It does NOT fully
	// close a cross-process TOCTOU window: two bd writes that both read
	// push-state.json before either writes the pre-stamp can both spawn a
	// worker. That race is bounded and self-limiting — dolt push is safe under
	// concurrency, so the worst case is one redundant push wasting CPU, and the
	// debounce stops the next wave. An OS-level lock (flock/LockFileEx) or a
	// running-worker PID check would close it; deferred as a follow-up on
	// gt-8cy3 unless redundant workers prove costly under write bursts.
	pushCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	debug.Logf("autopush worker: pushing (timeout 5m)...\n")
	pushErr := st.Push(pushCtx)

	// Record outcome to push-state.json. Both success and failure paths must
	// update LastPush so the parent's debounce check sees activity.
	ps, _ := loadPushState()
	if ps == nil {
		ps = &pushState{}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	ps.LastPush = now

	if pushErr == nil {
		// Success: refresh tracking fields. LastCommit comes from the store
		// (not the parent) because the worker may have pushed commits that
		// landed after the parent's fork.
		ps.LastSuccess = now
		ps.FailureStreak = 0
		ps.LastFailureReason = ""
		if cc, ccErr := st.GetCurrentCommit(pushCtx); ccErr == nil && cc != "" {
			ps.LastCommit = cc
		}
		debug.Logf("autopush worker: pushed successfully\n")
	} else {
		ps.FailureStreak++
		ps.LastFailureReason = truncateReason(pushErr.Error())
		debug.Logf("autopush worker: push error: %v\n", pushErr)
	}

	if err := savePushState(ps); err != nil {
		debug.Logf("autopush worker: failed to save push state: %v\n", err)
	}

	if pushErr != nil {
		// Non-zero exit so external monitors can detect failures, but the
		// parent never waits on us anyway — this is best-effort signaling.
		return pushErr
	}
	return nil
}
