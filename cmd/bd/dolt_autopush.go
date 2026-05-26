package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/storage"
)

// pushState tracks auto-push state in a local file (.beads/push-state.json)
// instead of the Dolt metadata table, to avoid merge conflicts on multi-machine
// setups (GH#2466).
//
// LastSuccess / FailureStreak / LastFailureReason let `gt dolt status` (and
// other observers) surface persistent autopush stalls. Without these, repeated
// timeouts silently throttle-and-retry forever; the only signal is a stderr
// warning that agent-invoked bd never sees.
type pushState struct {
	LastPush          string `json:"last_push"`                     // RFC3339 timestamp of most recent push attempt (success or failure)
	LastCommit        string `json:"last_commit"`                   // Dolt commit hash from last SUCCESSFUL push; left unchanged on failure so change-detection still triggers retries
	LastSuccess       string `json:"last_success,omitempty"`        // RFC3339 timestamp of last SUCCESSFUL push
	FailureStreak     int    `json:"failure_streak,omitempty"`      // consecutive failures since last success; cleared on success
	LastFailureReason string `json:"last_failure_reason,omitempty"` // error message from most recent failure (truncated)
}

// maxFailureReasonLen bounds LastFailureReason to keep push-state.json small.
const maxFailureReasonLen = 200

func pushStatePath() (string, error) {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return "", fmt.Errorf("%s", activeWorkspaceNotFoundError())
	}
	return filepath.Join(beadsDir, "push-state.json"), nil
}

func loadPushState() (*pushState, error) {
	path, err := pushStatePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed internally
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ps pushState
	if err := json.Unmarshal(data, &ps); err != nil {
		return nil, err
	}
	return &ps, nil
}

func savePushState(ps *pushState) error {
	path, err := pushStatePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data)
}

// isDoltAutoPushEnabled returns whether auto-push to Dolt remote should run.
// If user explicitly configured dolt.auto-push, use that.
// Otherwise, auto-enable when a Dolt remote named "origin" exists.
func isDoltAutoPushEnabled(ctx context.Context) bool {
	if config.GetValueSource("dolt.auto-push") != config.SourceDefault {
		return config.GetBool("dolt.auto-push")
	}
	// Auto-enable when a Dolt remote exists
	st := getStore()
	if st == nil {
		return false
	}
	if lm, ok := storage.UnwrapStore(st).(storage.LifecycleManager); ok && lm.IsClosed() {
		return false
	}
	has, err := st.HasRemote(ctx, "origin")
	if err != nil {
		debug.Logf("dolt auto-push: failed to check remote: %v\n", err)
		return false
	}
	return has
}

// maybeAutoPush pushes to the Dolt remote if enabled and the debounce interval has passed.
// Called from PersistentPostRun after auto-commit and auto-backup.
func maybeAutoPush(ctx context.Context) {
	if isSandboxMode() {
		debug.Logf("dolt auto-push: skipped (sandbox mode)\n")
		return
	}
	if !isDoltAutoPushEnabled(ctx) {
		return
	}

	st := getStore()
	if st == nil {
		return
	}
	if lm, ok := storage.UnwrapStore(st).(storage.LifecycleManager); ok && lm.IsClosed() {
		return
	}

	// Load local push state (file-based, not in Dolt metadata table).
	// This avoids merge conflicts on multi-machine setups (GH#2466).
	ps, err := loadPushState()
	if err != nil {
		debug.Logf("dolt auto-push: failed to load push state: %v\n", err)
		return
	}

	// Debounce: skip if we pushed recently
	interval := config.GetDuration("dolt.auto-push-interval")
	if interval == 0 {
		interval = 5 * time.Minute
	}

	if ps != nil && ps.LastPush != "" {
		lastPush, err := time.Parse(time.RFC3339, ps.LastPush)
		if err == nil && time.Since(lastPush) < interval {
			debug.Logf("dolt auto-push: throttled (last push %s ago, interval %s)\n",
				time.Since(lastPush).Round(time.Second), interval)
			return
		}
	}

	// Change detection: skip if nothing changed since last push
	currentCommit, err := st.GetCurrentCommit(ctx)
	if err != nil {
		debug.Logf("dolt auto-push: failed to get current commit: %v\n", err)
		return
	}
	if ps != nil && currentCommit == ps.LastCommit && ps.LastCommit != "" {
		debug.Logf("dolt auto-push: no changes since last push\n")
		return
	}

	// Fork a detached worker subprocess that performs the actual push (hq-8nkpj4).
	// The worker survives bd's exit, so the push isn't bounded by gt's 60s
	// bd-subprocess kill window — a clone that has accumulated more than a few
	// seconds of unpushed chunks can still auto-recover. The parent debounces
	// future triggers by pre-stamping LastPush before the fork, so concurrent
	// bd writes don't pile up workers.
	//
	// The previous inline path used a 30s timeout to fit under the kill window;
	// for clones that fell behind by more than 30s of upload (real workspaces
	// see this regularly), every autopush timed out and divergence grew
	// unbounded until writes started blocking on merge conflicts.
	if ps == nil {
		ps = &pushState{}
	}
	ps.LastPush = time.Now().UTC().Format(time.RFC3339)
	if saveErr := savePushState(ps); saveErr != nil {
		debug.Logf("dolt auto-push: failed to pre-stamp push state: %v\n", saveErr)
		// Continue anyway — debounce will be slightly weaker for one cycle.
	}

	if err := spawnAutopushWorker(); err != nil {
		// Spawn failure is rare (exec missing, fork limit hit). Don't surface
		// to user — autopush is best-effort and the next bd command will retry.
		debug.Logf("dolt auto-push: failed to spawn worker: %v\n", err)
		return
	}
	debug.Logf("dolt auto-push: dispatched detached worker\n")
}

// spawnAutopushWorker forks the hidden `bd dolt _autopush-worker` subcommand
// in a new process group so the push survives the parent's exit. Returns
// immediately; the worker is fire-and-forget.
func spawnAutopushWorker() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding bd executable: %w", err)
	}
	cmd := exec.Command(exe, "dolt", "_autopush-worker") //nolint:gosec // fixed subcommand
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = autopushDetachedAttr()
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	// Release the child immediately. Without this, the parent's process table
	// entry for the child accumulates until reap; Release decouples ownership.
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return nil
}

// truncateReason caps an error message at maxFailureReasonLen runes so
// push-state.json stays small even when a remote returns verbose error text.
func truncateReason(s string) string {
	if len(s) <= maxFailureReasonLen {
		return s
	}
	// Truncate at rune boundary, append ellipsis marker.
	runes := []rune(s)
	if len(runes) <= maxFailureReasonLen {
		return s
	}
	return string(runes[:maxFailureReasonLen]) + "…"
}
