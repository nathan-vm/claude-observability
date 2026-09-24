package wizard

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"claude-observability-wizard/internal/discovery"
)

func TestAskLine_EmptyReturnsDefault(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\n"))
	var out bytes.Buffer
	got := AskLine(&out, r, "OTel endpoint", "http://localhost:47317")
	if got != "http://localhost:47317" {
		t.Errorf("got %q, want default", got)
	}
}

func TestAskLine_InputOverridesDefault(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("http://example.com:9999\n"))
	var out bytes.Buffer
	got := AskLine(&out, r, "OTel endpoint", "http://localhost:47317")
	if got != "http://example.com:9999" {
		t.Errorf("got %q, want typed value", got)
	}
}

func TestAskYesNo_DefaultOnEmptyInput(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\n"))
	var out bytes.Buffer
	if !AskYesNo(&out, r, "Monitor all?", true) {
		t.Error("expected default true")
	}
}

func TestAskYesNo_ExplicitNo(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("n\n"))
	var out bytes.Buffer
	if AskYesNo(&out, r, "Monitor all?", true) {
		t.Error("expected false for explicit 'n'")
	}
}

func TestChooseAccounts_SingleAccountAutoChosen(t *testing.T) {
	accounts := []discovery.Account{{Dir: "/home/.claude", Email: "a@example.com"}}
	r := bufio.NewReader(strings.NewReader(""))
	var out bytes.Buffer
	chosen, rejected := ChooseAccounts(&out, r, accounts)
	if len(chosen) != 1 || len(rejected) != 0 {
		t.Errorf("chosen=%v rejected=%v, want single auto-chosen", chosen, rejected)
	}
}

func TestChooseAccounts_MonitorAll(t *testing.T) {
	accounts := []discovery.Account{
		{Dir: "/home/.claude", Email: "a@example.com"},
		{Dir: "/home/.claude-work", Email: "b@example.com"},
	}
	r := bufio.NewReader(strings.NewReader("y\n"))
	var out bytes.Buffer
	chosen, rejected := ChooseAccounts(&out, r, accounts)
	if len(chosen) != 2 || len(rejected) != 0 {
		t.Errorf("chosen=%d rejected=%d, want 2/0", len(chosen), len(rejected))
	}
}

func TestChooseAccounts_PerAccountChoice(t *testing.T) {
	accounts := []discovery.Account{
		{Dir: "/home/.claude", Email: "a@example.com"},
		{Dir: "/home/.claude-work", Email: "b@example.com"},
	}
	// "n" to "monitor all?", then "y" for the first, "n" for the second.
	r := bufio.NewReader(strings.NewReader("n\ny\nn\n"))
	var out bytes.Buffer
	chosen, rejected := ChooseAccounts(&out, r, accounts)
	if len(chosen) != 1 || chosen[0].Email != "a@example.com" {
		t.Errorf("chosen = %v, want [a@example.com]", chosen)
	}
	if len(rejected) != 1 || rejected[0].Email != "b@example.com" {
		t.Errorf("rejected = %v, want [b@example.com]", rejected)
	}
}
