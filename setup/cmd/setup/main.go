package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"claude-observability-setup/internal/discovery"
	"claude-observability-setup/internal/envwriter"
	"claude-observability-setup/internal/health"
	"claude-observability-setup/internal/limits"
	"claude-observability-setup/internal/service"
	"claude-observability-setup/internal/wizard"
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
	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}

	stdin := bufio.NewReader(os.Stdin)
	out := os.Stdout

	fmt.Fprintln(out, "claude-observability setup")
	fmt.Fprintln(out, "Configures Claude Code telemetry and the collector service. Does NOT start")
	fmt.Fprintln(out, "docker for you — bring the stack up yourself first, then run this.")

	fmt.Fprintln(out, "\n── Which accounts to monitor ──────────────────────────────")
	accounts, err := discovery.Find(home)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Fprintln(out, "  No config directory found (~/.claude*/projects).")
		fmt.Fprintln(out, "  Falling back to ~/.claude — re-run this after your first Claude Code session.")
		accounts = []discovery.Account{{Dir: filepath.Join(home, ".claude")}}
	}
	chosen, rejected := wizard.ChooseAccounts(out, stdin, accounts)

	fmt.Fprintln(out, "\n── OTel endpoint ───────────────────────────────────────────")
	endpoint := wizard.AskLine(out, stdin, "OTel endpoint", "http://localhost:47317")
	token := wizard.AskLine(out, stdin, "OTel token", "")

	fmt.Fprintf(out, "  checking %s ... ", endpoint)
	if err := health.Dial(endpoint, 3*time.Second); err != nil {
		fmt.Fprintln(out, "unreachable")
		if endpoint == "http://localhost:47317" {
			return fmt.Errorf("can't reach %s — nothing is listening there.\n  Run `docker compose up -d` first, then re-run this wizard", endpoint)
		}
		return fmt.Errorf("can't reach %s — nothing is listening there.\n  Check the URL and that it's reachable from this machine", endpoint)
	}
	fmt.Fprintln(out, "ok")

	telemetryVars := []envwriter.Var{
		{Name: "CLAUDE_CODE_ENABLE_TELEMETRY", Value: "1"},
		{Name: "OTEL_METRICS_EXPORTER", Value: "otlp"},
		{Name: "OTEL_LOGS_EXPORTER", Value: "otlp"},
		{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "grpc"},
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: endpoint},
		{Name: "OTEL_METRIC_EXPORT_INTERVAL", Value: "60000"},
		{Name: "OTEL_LOGS_EXPORT_INTERVAL", Value: "30000"},
	}
	if token != "" {
		telemetryVars = append(telemetryVars, envwriter.Var{
			Name:  "OTEL_EXPORTER_OTLP_HEADERS",
			Value: "Authorization=Bearer%20" + token,
		})
	}

	var collectorVars []envwriter.Var
	if len(chosen) > 0 {
		collectorVars = append(collectorVars, envwriter.Var{Name: "CLAUDE_DIR", Value: chosen[0].Dir})
		if len(chosen) > 1 {
			extra := chosen[1].Dir
			for _, a := range chosen[2:] {
				extra += ":" + a.Dir
			}
			collectorVars = append(collectorVars, envwriter.Var{Name: "CLAUDE_OBSERVABILITY_EXTRA_DIRS", Value: extra})
		}
		collectorVars = append(collectorVars, envwriter.Var{Name: "EXPORTER_STREAM", Value: "claude-code-exporter-1"})
	}

	allVars := append(append([]envwriter.Var{}, telemetryVars...), collectorVars...)

	fmt.Fprintln(out, "\n── Enabling telemetry in your shell ───────────────────────")
	if err := writeShellConfig(out, home, allVars); err != nil {
		return err
	}

	if len(rejected) > 0 {
		fmt.Fprintln(out, "\n── Updating ignore list ────────────────────────────────────")
		var emails []string
		for _, a := range rejected {
			if a.Email != "" {
				emails = append(emails, a.Email)
			}
		}
		limitsPath := filepath.Join(repoRoot, "grafana", "account-limits.json")
		examplePath := filepath.Join(repoRoot, "grafana", "account-limits.example.json")
		if err := limits.Update(limitsPath, examplePath, emails); err != nil {
			return err
		}
		fmt.Fprintf(out, "  not monitored: %v\n", emails)
	}

	fmt.Fprintln(out, "\n── Collector service ───────────────────────────────────────")
	if wizard.AskYesNo(out, stdin, "  Install the collector as a background service now?", true) {
		if err := installService(repoRoot, collectorVars); err != nil {
			return fmt.Errorf("installing collector service: %w", err)
		}
		fmt.Fprintln(out, "  installed and started")
	}

	fmt.Fprintln(out, "\n── Done ─────────────────────────────────────────────────────")
	fmt.Fprintln(out, "  Open a new terminal (or reload your shell config) to pick up the telemetry vars.")
	return nil
}

// findRepoRoot requires the binary to be run from the claude-observability
// repo root — it doesn't bundle docker-compose.yaml/collector/grafana, so
// those still have to come from the checkout it's invoked inside.
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

func writeShellConfig(out *os.File, home string, vars []envwriter.Var) error {
	if runtime.GOOS == "windows" {
		return envwriter.WriteWindows(vars, func(name, value string) error {
			return exec.Command("setx", name, value).Run()
		})
	}

	shellName := filepath.Base(os.Getenv("SHELL"))
	var path string
	var shell envwriter.Shell
	switch shellName {
	case "fish":
		path = filepath.Join(home, ".config", "fish", "config.fish")
		shell = envwriter.Fish
	case "zsh":
		path = filepath.Join(home, ".zshrc")
		shell = envwriter.Bash
	default:
		path = filepath.Join(home, ".bashrc")
		shell = envwriter.Bash
	}

	wrote, err := envwriter.WriteBlock(path, shell, vars)
	if err != nil {
		return err
	}
	if wrote {
		fmt.Fprintf(out, "  telemetry + collector env vars added to %s\n", path)
	} else {
		fmt.Fprintf(out, "  already present in %s (edit the block by hand to change accounts)\n", path)
	}
	return nil
}

func installService(repoRoot string, collectorVars []envwriter.Var) error {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		return fmt.Errorf("node not found on PATH: %w", err)
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found on PATH: %w", err)
	}
	cfg := service.Config{
		RepoRoot:  repoRoot,
		NodeBin:   nodeBin,
		ClaudeBin: claudeBin,
		Env:       collectorVars,
		LogDir:    filepath.Join(repoRoot, ".state"),
	}
	return service.Install(cfg)
}
