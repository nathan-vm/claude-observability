package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"claude-observability-wizard/internal/claudesettings"
	"claude-observability-wizard/internal/discovery"
	"claude-observability-wizard/internal/envwriter"
	"claude-observability-wizard/internal/health"
	"claude-observability-wizard/internal/limits"
	"claude-observability-wizard/internal/service"
	"claude-observability-wizard/internal/streamid"
	"claude-observability-wizard/internal/wizard"
)

var version = "dev"

func main() {
	if hasArg("--version") {
		fmt.Println(version)
		return
	}
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
	accounts = discovery.WithConfigDir(accounts, os.Getenv("CLAUDE_CONFIG_DIR"))
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

	settingsVars, telemetryVars := buildTelemetryVars(endpoint, token)

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
		// See resolveExporterStream: reuses this install's existing
		// EXPORTER_STREAM if one is already configured, so a wizard re-run
		// never orphans the collector's existing Loki stream. Only mints a
		// fresh id (internal/streamid) for a genuinely new install.
		streamValue, err := resolveExporterStream(home)
		if err != nil {
			return err
		}
		collectorVars = append(collectorVars, envwriter.Var{Name: "EXPORTER_STREAM", Value: streamValue})
	}

	allVars := append(append([]envwriter.Var{}, telemetryVars...), collectorVars...)

	fmt.Fprintln(out, "\n── Enabling telemetry in your shell ───────────────────────")
	if warning := staleEndpointWarning(home, endpoint); warning != "" {
		fmt.Fprintln(out, warning)
	}
	if err := writeShellConfig(out, home, allVars); err != nil {
		return err
	}

	var updatedSettings, skippedSettings []string
	if targets := settingsTargets(chosen); len(targets) > 0 {
		fmt.Fprintln(out, "\n── Enabling telemetry in Claude Code settings ───────────────")
		fmt.Fprintln(out, "  Sessions started without your shell rc (GUI launchers such as Maestro) only")
		fmt.Fprintln(out, "  see telemetry settings from settings.json. These files will get an \"env\" entry:")
		for _, dir := range targets {
			fmt.Fprintf(out, "    %s\n", settingsPath(dir))
		}
		if wizard.AskYesNo(out, stdin, "  Also enable telemetry in these Claude Code settings files (existing values of these env keys are overwritten)?", true) {
			updatedSettings, skippedSettings = writeClaudeSettings(out, targets, settingsVars)
			if token != "" {
				fmt.Fprintln(out, "  note: the auth token (OTEL_EXPORTER_OTLP_HEADERS) is not written to settings.json;")
				fmt.Fprintln(out, "  sessions that don't read your shell rc will not authenticate against a gateway-protected endpoint.")
			}
		}
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
		if err := limits.Update(limitsPath, emails); err != nil {
			return err
		}
		fmt.Fprintf(out, "  not monitored: %v\n", emails)
	}

	fmt.Fprintln(out, "\n── Collector ─────────────────────────────────────────────────")
	if wizard.AskYesNo(out, stdin, "  Install the collector as a background service now?", true) {
		if err := installCollectorService(repoRoot, collectorVars); err != nil {
			return fmt.Errorf("installing collector service: %w", err)
		}
		fmt.Fprintln(out, "  installed and started")
	}

	fmt.Fprintln(out, "\n── Done ─────────────────────────────────────────────────────")
	fmt.Fprintln(out, "  Open a new terminal (or reload your shell config) to pick up the telemetry vars.")
	if len(updatedSettings) > 0 {
		fmt.Fprintln(out, "  Restart any Claude Code session or launcher already running (e.g. Maestro) to pick up settings.json.")
	}
	if len(skippedSettings) > 0 {
		fmt.Fprintln(out, "  Skipped settings files (fix and re-run):")
		for _, p := range skippedSettings {
			fmt.Fprintf(out, "    %s\n", p)
		}
	}
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

// shellRCPath returns the rc file this OS/shell combination uses for
// persistent env vars, and which Shell syntax to render into it. Shared by
// writeShellConfig (writing) and resolveExporterStream (peeking at an
// existing value before writing) so both always agree on which file "this
// run" means.
func shellRCPath(home string) (path string, shell envwriter.Shell) {
	shellName := filepath.Base(os.Getenv("SHELL"))
	switch shellName {
	case "fish":
		return filepath.Join(home, ".config", "fish", "config.fish"), envwriter.Fish
	case "zsh":
		return filepath.Join(home, ".zshrc"), envwriter.Bash
	default:
		return filepath.Join(home, ".bashrc"), envwriter.Bash
	}
}

// buildTelemetryVars returns the vars for settings.json (settingsVars) and
// the superset for the rc file / Windows environment (telemetryVars).
// OTEL_EXPORTER_OTLP_HEADERS (the auth token) is deliberately only in the
// latter: settings.json is a plaintext file other tools may read or sync, so
// the token is not copied into it.
func buildTelemetryVars(endpoint, token string) (settingsVars, telemetryVars []envwriter.Var) {
	settingsVars = []envwriter.Var{
		{Name: "CLAUDE_CODE_ENABLE_TELEMETRY", Value: "1"},
		{Name: "OTEL_METRICS_EXPORTER", Value: "otlp"},
		{Name: "OTEL_LOGS_EXPORTER", Value: "otlp"},
		{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "grpc"},
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: endpoint},
		{Name: "OTEL_METRIC_EXPORT_INTERVAL", Value: "60000"},
		{Name: "OTEL_LOGS_EXPORT_INTERVAL", Value: "30000"},
	}
	telemetryVars = append([]envwriter.Var{}, settingsVars...)
	if token != "" {
		telemetryVars = append(telemetryVars, envwriter.Var{
			Name:  "OTEL_EXPORTER_OTLP_HEADERS",
			Value: "Authorization=Bearer%20" + token,
		})
	}
	return settingsVars, telemetryVars
}

func settingsPath(dir string) string {
	return filepath.Join(dir, "settings.json")
}

// settingsTargets returns the config dirs of the accounts the user chose, in
// order and without duplicates. Rejected accounts never appear here.
func settingsTargets(chosen []discovery.Account) []string {
	seen := make(map[string]bool, len(chosen))
	var dirs []string
	for _, a := range chosen {
		dir := filepath.Clean(a.Dir)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	return dirs
}

// writeClaudeSettings merges vars into <dir>/settings.json for each dir and
// reports every existing value it overwrites. A failure on one dir is
// reported and skipped, never fatal. It returns the settings.json paths that
// were rewritten and those that were skipped.
func writeClaudeSettings(out io.Writer, dirs []string, vars []envwriter.Var) (updated, skipped []string) {
	for _, dir := range dirs {
		path := settingsPath(dir)
		changed, overwritten, err := claudesettings.MergeEnv(path, vars)
		switch {
		case err != nil:
			fmt.Fprintf(out, "  skipped %s: %v (file left untouched - fix it and re-run)\n", path, err)
			skipped = append(skipped, path)
		case changed:
			fmt.Fprintf(out, "  updated %s\n", path)
			for _, o := range overwritten {
				fmt.Fprintf(out, "    overwrote %s: %s -> %s\n", o.Name, displayForLog(o.Name, o.Old), displayForLog(o.Name, o.New))
			}
			updated = append(updated, path)
		default:
			fmt.Fprintf(out, "  already up to date: %s\n", path)
		}
	}
	return updated, skipped
}

func displayForLog(name, value string) string {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "KEY", "AUTH", "HEADERS"} {
		if strings.Contains(upper, marker) {
			return "<redacted>"
		}
	}
	return value
}

// staleEndpointWarning returns a warning when the existing rc block already
// pins a different OTEL_EXPORTER_OTLP_ENDPOINT than the one just chosen (the
// rc block is write-once, so it won't be rewritten), or "" otherwise. Always
// "" on Windows, where setx overwrites.
func staleEndpointWarning(home, endpoint string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	path, shell := shellRCPath(home)
	existing, ok, err := envwriter.ExistingVar(path, shell, "OTEL_EXPORTER_OTLP_ENDPOINT")
	if err != nil || !ok || existing == endpoint {
		return ""
	}
	return fmt.Sprintf("  warning: %s still has OTEL_EXPORTER_OTLP_ENDPOINT=%s, not the endpoint you entered (%s);\n  the rc block is not rewritten on re-run - edit it by hand.", path, existing, endpoint)
}

func writeShellConfig(out *os.File, home string, vars []envwriter.Var) error {
	if runtime.GOOS == "windows" {
		return envwriter.WriteWindows(vars, func(name, value string) error {
			return exec.Command("setx", name, value).Run()
		})
	}

	path, shell := shellRCPath(home)

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

// resolveExporterStreamValue returns existing unchanged if it's non-empty
// (an already-configured install's stream id, found by resolveExporterStream
// below), or mints a fresh "claude-code-exporter-<uuid>" value otherwise (a
// genuinely new install).
func resolveExporterStreamValue(existing string) (string, error) {
	if existing != "" {
		return existing, nil
	}
	id, err := streamid.New()
	if err != nil {
		return "", err
	}
	return "claude-code-exporter-" + id, nil
}

// resolveExporterStream returns the EXPORTER_STREAM value this run should
// use: the id already committed for this install, if one is found, so a
// wizard re-run never orphans the collector's existing Loki stream — a
// fresh id (see resolveExporterStreamValue) is minted only when none is
// found, i.e. a genuinely new install.
//
// On Unix, the source of truth is the shell rc file this run would write to
// (shellRCPath) — not the installed collector service definition (launchd
// plist / systemd unit), even though that's what actually determines the
// running collector's env: the service definition is exactly the artifact
// this bug corrupts, so trusting it as a source would perpetuate any
// existing drift instead of healing it. The shell rc file is also literally
// what a manual `docker compose up` reads EXPORTER_STREAM from at
// compose-up time, so it's the more authoritative side to trust anyway. A
// side effect: running this fixed wizard on a machine that already drifted
// (mismatched shell rc vs. service definition, from a prior buggy run)
// re-derives the correct id from the shell rc and rewrites the service
// definition back into agreement.
//
// On Windows there's no persisted marker file (envwriter.WriteWindows calls
// `setx`, which always overwrites and keeps no marker — see envwriter), so
// the only available signal is this process's own environment: reused if
// the current session already picked up a prior `setx` (true once the user
// has opened a new terminal since installing), minted fresh otherwise — the
// same reload caveat CLAUDE_DIR already has today on Windows.
//
// Not handled, pre-existing and unchanged by this fix: switching shells
// between wizard runs (e.g. bash -> fish) writes a second, separate rc
// block with its own fresh id, same as it always has; and an accounts-less
// first run (no CLAUDE_DIR/EXPORTER_STREAM ever written, since
// collectorVars is only populated when len(chosen) > 0) is treated as
// "nothing to reuse yet" the first time accounts are later found — correct,
// since the collector is only being configured for the first time then.
func resolveExporterStream(home string) (string, error) {
	if runtime.GOOS == "windows" {
		return resolveExporterStreamValue(os.Getenv("EXPORTER_STREAM"))
	}
	path, shell := shellRCPath(home)
	existing, ok, err := envwriter.ExistingVar(path, shell, "EXPORTER_STREAM")
	if err != nil {
		return "", err
	}
	if !ok {
		existing = ""
	}
	return resolveExporterStreamValue(existing)
}

func installCollectorService(repoRoot string, collectorVars []envwriter.Var) error {
	collectorBin, err := findCollectorBinary(repoRoot)
	if err != nil {
		return err
	}
	cfg := service.Config{
		Label:      collectorLabel(),
		Command:    collectorBin,
		WorkingDir: repoRoot,
		Env:        collectorVars,
		LogDir:     filepath.Join(repoRoot, ".state"),
		// The collector shells out to `claude` (for usage-truth) with only
		// this process's own PATH — launchd/systemd don't source a shell rc,
		// so a `claude` installed somewhere other than the OS-standard bin
		// dirs (Homebrew on Apple Silicon: /opt/homebrew/bin, nvm, a global
		// npm prefix, ...) would otherwise 404. Resolve it once, here, from
		// the wizard's own (interactive, real) PATH and bake its directory
		// in alongside the standard ones.
		ExtraPathDirs: claudeBinDir(),
	}
	return service.Install(cfg)
}

// claudeBinDir returns the directory containing the `claude` CLI as found on
// this process's own PATH, or "" if it isn't found (the service will still
// install — just without usage-truth working until `claude` is reachable).
func claudeBinDir() string {
	path, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	return filepath.Dir(path)
}

// findCollectorBinary looks for a collector binary built alongside this
// one (same directory as the running setup executable) or on PATH.
func findCollectorBinary(repoRoot string) (string, error) {
	name := "claude-observability-collector"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), name)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("%s not found next to this binary or on PATH — build it from src/collector (go build -o %s ./cmd/collector) or download it alongside claude-observability-wizard", name, name)
}

func collectorLabel() string {
	switch runtime.GOOS {
	case "darwin":
		return "com.claude-observability.collector"
	case "windows":
		return "ClaudeObservabilityCollector"
	default:
		return "claude-observability-collector"
	}
}

func hasArg(name string) bool {
	for _, a := range os.Args[1:] {
		if a == name {
			return true
		}
	}
	return false
}
