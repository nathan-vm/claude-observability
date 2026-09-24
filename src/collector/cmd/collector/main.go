package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"claude-observability-collector/internal/accounts"
	"claude-observability-collector/internal/transcriptscan"
	"claude-observability-collector/internal/usagetruth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		return err
	}

	configDirs := accounts.ResolveConfigDirs(home, os.Getenv("CLAUDE_DIR"), os.Getenv("CLAUDE_OBSERVABILITY_EXTRA_DIRS"))
	projectDirs := make([]string, len(configDirs))
	for i, d := range configDirs {
		projectDirs[i] = filepath.Join(d, "projects")
	}

	lokiURL := envOr("LOKI_URL", "http://localhost:47100")
	// Real installs always have this set by the wizard (a random id per
	// install, see src/wizard/internal/streamid) — this fallback only
	// fires for an ad hoc run outside that flow (dev/test), so it doesn't
	// need to match any specific value, only the same one dash-generator
	// falls back to.
	exporterStream := envOr("EXPORTER_STREAM", "claude-code-exporter-dev")
	pollSeconds := envInt("POLL_SECONDS", 60)
	statePath := envOr("STATE_FILE", filepath.Join(repoRoot, ".state", "collector-state.json"))

	once := hasArg("--once")
	dryRun := hasArg("--dry-run")
	rescan := hasArg("--rescan")

	log := func(format string, args ...any) { fmt.Printf(time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...) }

	cfg := transcriptscan.Config{
		ConfigDirs: projectDirs, LokiURL: lokiURL, ExporterStream: exporterStream,
		BatchSize: envInt("BATCH_SIZE", 2000), OrphanAfterMs: int64(envInt("ORPHAN_AFTER_MS", 15*60*1000)),
		DedupDays: envInt("DEDUP_DAYS", 90), EmailLookbackHours: envInt("EMAIL_LOOKBACK_HOURS", 720),
		DryRun: dryRun, Rescan: rescan, StatePath: statePath, Log: log,
	}

	st, err := transcriptscan.InitState(cfg)
	if err != nil {
		return err
	}

	pass := func() error {
		count, err := transcriptscan.RunPass(cfg, st)
		if err != nil {
			log("transcript scan failed: %v", err)
		} else if count > 0 {
			log("%d tool call(s) exported", count)
		}
		if err := usagetruth.PublishUsageTruth(lokiURL, configDirs, log); err != nil {
			log("usage-truth failed: %v", err)
		}
		return nil
	}

	if once {
		return pass()
	}
	for {
		if err := pass(); err != nil {
			return err
		}
		time.Sleep(time.Duration(pollSeconds) * time.Second)
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func hasArg(name string) bool {
	for _, a := range os.Args[1:] {
		if a == name {
			return true
		}
	}
	return false
}
