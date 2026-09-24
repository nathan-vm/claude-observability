package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"claude-observability-dash-generator/internal/dashboardgen"
	"claude-observability-dash-generator/internal/ratemeter"
	"claude-observability-dash-generator/internal/state"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	grafanaDir := os.Getenv("GRAFANA_DIR")
	statePath := os.Getenv("STATE_FILE")
	if grafanaDir == "" || statePath == "" {
		// Local/dev use: no server yet to run this against, so it's driven
		// by hand from a checkout — infer both from the repo root instead of
		// requiring every env var. In the container (docker-compose.yaml's
		// dash-generator service, standing in for "the server"), both are
		// set explicitly and this branch never runs.
		repoRoot, err := findRepoRoot()
		if err != nil {
			return err
		}
		if grafanaDir == "" {
			grafanaDir = filepath.Join(repoRoot, "grafana")
		}
		if statePath == "" {
			statePath = filepath.Join(repoRoot, ".state", "dash-generator-state.json")
		}
	}

	lokiURL := envOr("LOKI_URL", "http://localhost:47100")
	exporterStream := envOr("EXPORTER_STREAM", "claude-code-exporter-1")
	rateHalfLife := envOr("RATE_HALFLIFE", "20m")
	halfLifeS, err := parseHalfLife(rateHalfLife)
	if err != nil {
		return err
	}
	backfillDays := envInt("RATE_BACKFILL_DAYS", 14)
	pollSeconds := envInt("POLL_SECONDS", 60)
	dashboardIntervalSeconds := envInt("DASHBOARD_INTERVAL_SECONDS", 600)

	once := hasArg("--once")
	dryRun := hasArg("--dry-run")

	st, err := state.Load(statePath)
	if err != nil {
		return err
	}

	log := func(format string, args ...any) { fmt.Printf(time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...) }

	ratePass := func() error {
		n, err := ratemeter.PublishRate(ratemeter.PublishConfig{
			LokiURL: lokiURL, HalfLifeS: halfLifeS, HalfLifeLabel: rateHalfLife,
			BackfillDays: backfillDays, DryRun: dryRun,
		}, st, log)
		if err != nil {
			log("rate meter failed: %v", err)
			return nil
		}
		if n > 0 {
			log("%d rate point(s) published", n)
		}
		if !dryRun {
			return state.Save(statePath, st)
		}
		return nil
	}

	dashboardPass := func() error {
		if dryRun {
			return nil
		}
		if err := dashboardgen.GenerateDashboards(dashboardgen.Config{
			LokiURL: lokiURL, ExporterStream: exporterStream, RateHalfLife: rateHalfLife, GrafanaDir: grafanaDir,
		}, log); err != nil {
			log("dashboard generation failed: %v", err)
		}
		return nil
	}

	if once {
		if err := ratePass(); err != nil {
			return err
		}
		return dashboardPass()
	}

	errCh := make(chan error, 2)
	go func() { errCh <- loop(pollSeconds, ratePass) }()
	go func() { errCh <- loop(dashboardIntervalSeconds, dashboardPass) }()
	return <-errCh
}

func loop(intervalSeconds int, run func() error) error {
	for {
		if err := run(); err != nil {
			return err
		}
		time.Sleep(time.Duration(intervalSeconds) * time.Second)
	}
}

// findRepoRoot requires the binary to be run from the claude-observability
// repo root — it doesn't bundle grafana/templates or docker-compose.yaml,
// so those still have to come from the checkout it's invoked inside.
func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(wd, "docker-compose.yaml")); err != nil {
		return "", fmt.Errorf("must be run from the claude-observability repo root (docker-compose.yaml not found in %s)", wd)
	}
	return wd, nil
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

var halfLifePattern = regexp.MustCompile(`^(\d+)([smh])$`)

func parseHalfLife(value string) (int64, error) {
	m := halfLifePattern.FindStringSubmatch(value)
	if m == nil {
		return 0, fmt.Errorf("RATE_HALFLIFE must look like 20m, 90s or 2h (got %q)", value)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	switch m[2] {
	case "s":
		return n, nil
	case "m":
		return n * 60, nil
	case "h":
		return n * 3600, nil
	}
	return 0, fmt.Errorf("unreachable")
}
