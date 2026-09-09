package dolt

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/configfile"
)

// TestApplyResolvedConfigReadsPoolTimeoutsFromDir pins the config.yaml rung of
// the pool-deadline ladder for library consumers. The config.GetString rungs
// read a package-global viper populated only by cmd/bd's config.Initialize(),
// so for a process that links beads as a library they always return "" and the
// project's configured pool deadlines were silently ignored — the process ran
// the 10s default whatever config.yaml said. applyResolvedConfig now falls
// back to a direct read of <beadsDir>/config.yaml, the same pattern
// dolt.auto-start carries for this exact hole.
func TestApplyResolvedConfigReadsPoolTimeoutsFromDir(t *testing.T) {
	writeConfigYAML := func(t *testing.T, beadsDir, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatalf("writing config.yaml: %v", err)
		}
	}
	baseFileCfg := func() *configfile.Config {
		return &configfile.Config{Backend: configfile.BackendDolt, DoltDatabase: "ladder"}
	}

	t.Run("config.yaml populates the deadlines with no env and no global viper", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_POOL_READ_TIMEOUT", "")
		t.Setenv("BEADS_DOLT_POOL_WRITE_TIMEOUT", "")
		beadsDir := t.TempDir()
		writeConfigYAML(t, beadsDir, "dolt:\n  pool-read-timeout: 120s\n  pool-write-timeout: 45\n")
		cfg := &Config{}

		if err := applyResolvedConfig(context.Background(), beadsDir, baseFileCfg(), cfg); err != nil {
			t.Fatalf("applyResolvedConfig: %v", err)
		}

		if cfg.PoolReadTimeout != 120*time.Second {
			t.Fatalf("PoolReadTimeout = %v, want 120s from config.yaml", cfg.PoolReadTimeout)
		}
		if cfg.PoolWriteTimeout != 45*time.Second {
			t.Fatalf("PoolWriteTimeout = %v, want 45s from config.yaml (bare number = seconds)", cfg.PoolWriteTimeout)
		}
	})

	t.Run("env var wins over config.yaml", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_POOL_READ_TIMEOUT", "90s")
		t.Setenv("BEADS_DOLT_POOL_WRITE_TIMEOUT", "")
		beadsDir := t.TempDir()
		writeConfigYAML(t, beadsDir, "dolt:\n  pool-read-timeout: 120s\n")
		cfg := &Config{}

		if err := applyResolvedConfig(context.Background(), beadsDir, baseFileCfg(), cfg); err != nil {
			t.Fatalf("applyResolvedConfig: %v", err)
		}

		if cfg.PoolReadTimeout != 90*time.Second {
			t.Fatalf("PoolReadTimeout = %v, want the env's 90s over the file's 120s", cfg.PoolReadTimeout)
		}
	})

	t.Run("no config.yaml leaves the deadlines unset", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_POOL_READ_TIMEOUT", "")
		t.Setenv("BEADS_DOLT_POOL_WRITE_TIMEOUT", "")
		cfg := &Config{}

		if err := applyResolvedConfig(context.Background(), t.TempDir(), baseFileCfg(), cfg); err != nil {
			t.Fatalf("applyResolvedConfig: %v", err)
		}

		if cfg.PoolReadTimeout != 0 || cfg.PoolWriteTimeout != 0 {
			t.Fatalf("pool deadlines = %v/%v, want 0/0 so buildServerDSN applies its default", cfg.PoolReadTimeout, cfg.PoolWriteTimeout)
		}
	})
}
