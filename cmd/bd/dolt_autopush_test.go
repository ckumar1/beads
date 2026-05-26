package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/config"
)

func TestIsDoltAutoPushEnabled_ExplicitConfig(t *testing.T) {
	// Cannot be parallel: modifies global env vars and config.

	tests := []struct {
		name       string
		envVal     string // "true"/"false" = explicit config via env
		wantResult bool
	}{
		{
			name:       "explicit true → enabled",
			envVal:     "true",
			wantResult: true,
		},
		{
			name:       "explicit false → disabled",
			envVal:     "false",
			wantResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BD_DOLT_AUTO_PUSH", tt.envVal)

			config.ResetForTesting()
			t.Cleanup(func() { config.ResetForTesting() })
			if err := config.Initialize(); err != nil {
				t.Fatalf("config.Initialize: %v", err)
			}

			// With explicit config, store check is bypassed
			// (store is nil in this test, which would return false for auto-detection)
			got := isDoltAutoPushEnabled(context.Background())
			if got != tt.wantResult {
				t.Errorf("isDoltAutoPushEnabled() = %v, want %v", got, tt.wantResult)
			}
		})
	}
}

func TestIsDoltAutoPushEnabled_DefaultNoStore(t *testing.T) {
	// When no explicit config and no store, should return false.
	os.Unsetenv("BD_DOLT_AUTO_PUSH")
	t.Cleanup(func() { os.Unsetenv("BD_DOLT_AUTO_PUSH") })

	config.ResetForTesting()
	t.Cleanup(func() { config.ResetForTesting() })
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}

	// store is nil → auto-detection returns false
	got := isDoltAutoPushEnabled(context.Background())
	if got != false {
		t.Errorf("isDoltAutoPushEnabled() with nil store = %v, want false", got)
	}
}

func TestMaybeAutoPush_NilStore(t *testing.T) {
	// maybeAutoPush should be a no-op when store is nil (no panic).
	os.Unsetenv("BD_DOLT_AUTO_PUSH")
	t.Cleanup(func() { os.Unsetenv("BD_DOLT_AUTO_PUSH") })

	config.ResetForTesting()
	t.Cleanup(func() { config.ResetForTesting() })
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}

	// Should not panic with nil store
	maybeAutoPush(context.Background())
}

func TestAutoPush_SkippedForReadOnlyCommands(t *testing.T) {
	// Read-only commands should not trigger auto-push (GH#2191).
	readOnly := []string{"list", "ready", "show", "stats", "blocked", "search", "graph"}
	for _, cmd := range readOnly {
		if !isReadOnlyCommand(cmd) {
			t.Errorf("isReadOnlyCommand(%q) = false, want true", cmd)
		}
	}

	writeCmds := []string{"create", "update", "close", "import"}
	for _, cmd := range writeCmds {
		if isReadOnlyCommand(cmd) {
			t.Errorf("isReadOnlyCommand(%q) = true, want false", cmd)
		}
	}
}

func TestMaybeAutoPush_DisabledByConfig(t *testing.T) {
	// When explicitly disabled, maybeAutoPush should be a no-op.
	t.Setenv("BD_DOLT_AUTO_PUSH", "false")

	config.ResetForTesting()
	t.Cleanup(func() { config.ResetForTesting() })
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}

	// Should not panic or attempt push
	maybeAutoPush(context.Background())
}

func TestLoadSavePushState(t *testing.T) {

	// Create a temp .beads dir with metadata.json so FindBeadsDir works
	tmp := t.TempDir()
	beadsDir := filepath.Join(tmp, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DIR", beadsDir)

	// No file yet → nil, nil
	ps, err := loadPushState()
	if err != nil {
		t.Fatalf("loadPushState (no file): %v", err)
	}
	if ps != nil {
		t.Fatalf("loadPushState (no file): got %+v, want nil", ps)
	}

	// Save and reload
	want := &pushState{LastPush: "2026-03-09T12:00:00Z", LastCommit: "abc123"}
	if err := savePushState(want); err != nil {
		t.Fatalf("savePushState: %v", err)
	}
	got, err := loadPushState()
	if err != nil {
		t.Fatalf("loadPushState: %v", err)
	}
	if got == nil || got.LastPush != want.LastPush || got.LastCommit != want.LastCommit {
		t.Errorf("loadPushState = %+v, want %+v", got, want)
	}
}

func TestPushState_AllFieldsRoundTrip(t *testing.T) {
	// The async worker writes LastSuccess / FailureStreak / LastFailureReason
	// in addition to the original LastPush / LastCommit. Ensure every field
	// survives a save/load cycle so gt dolt status reads accurate values.
	tmp := t.TempDir()
	beadsDir := filepath.Join(tmp, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DIR", beadsDir)

	want := &pushState{
		LastPush:          "2026-05-26T12:00:00Z",
		LastCommit:        "abc123",
		LastSuccess:       "2026-05-26T11:59:00Z",
		FailureStreak:     3,
		LastFailureReason: "context deadline exceeded",
	}
	if err := savePushState(want); err != nil {
		t.Fatalf("savePushState: %v", err)
	}
	got, err := loadPushState()
	if err != nil {
		t.Fatalf("loadPushState: %v", err)
	}
	if got == nil {
		t.Fatal("loadPushState: got nil")
	}
	if *got != *want {
		t.Errorf("loadPushState = %+v, want %+v", got, want)
	}
}

func TestTruncateReason(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "short string unchanged",
			in:   "boom",
			want: "boom",
		},
		{
			name: "exactly max length unchanged",
			in:   strings.Repeat("a", maxFailureReasonLen),
			want: strings.Repeat("a", maxFailureReasonLen),
		},
		{
			name: "long ascii truncated to max runes plus ellipsis",
			in:   strings.Repeat("a", maxFailureReasonLen+50),
			want: strings.Repeat("a", maxFailureReasonLen) + "…",
		},
		{
			// Byte length > max but rune count <= max: must NOT truncate.
			name: "multibyte under rune limit unchanged",
			in:   strings.Repeat("世", 150), // 150 runes, 450 bytes
			want: strings.Repeat("世", 150),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateReason(tt.in)
			if got != tt.want {
				t.Errorf("truncateReason() len=%d, want len=%d", utf8.RuneCountInString(got), utf8.RuneCountInString(tt.want))
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateReason() produced invalid UTF-8: %q", got)
			}
		})
	}

	t.Run("long multibyte truncated at rune boundary", func(t *testing.T) {
		in := strings.Repeat("世", maxFailureReasonLen+10) // 210 runes
		got := truncateReason(in)
		if !utf8.ValidString(got) {
			t.Fatalf("truncateReason produced invalid UTF-8")
		}
		// maxFailureReasonLen runes + the single-rune ellipsis.
		if want := maxFailureReasonLen + 1; utf8.RuneCountInString(got) != want {
			t.Errorf("rune count = %d, want %d", utf8.RuneCountInString(got), want)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("expected ellipsis suffix, got %q", got)
		}
	})
}

func TestLoadPushState_CorruptJSON(t *testing.T) {

	tmp := t.TempDir()
	beadsDir := filepath.Join(tmp, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DIR", beadsDir)

	// Write garbage
	if err := os.WriteFile(filepath.Join(beadsDir, "push-state.json"), []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadPushState()
	if err == nil {
		t.Error("loadPushState with corrupt JSON: expected error, got nil")
	}
}
