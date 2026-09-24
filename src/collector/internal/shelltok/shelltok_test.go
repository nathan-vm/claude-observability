package shelltok

import (
	"reflect"
	"testing"
)

func TestExtractBashCommands_SimpleAndChained(t *testing.T) {
	got := ExtractBashCommands("git status && ls -la")
	want := []string{"git", "ls"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_PipeAndSemicolonAndNewline(t *testing.T) {
	got := ExtractBashCommands("cat foo.txt | grep bar; echo done\npwd")
	want := []string{"cat", "grep", "echo", "pwd"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_SkipsEnvAssignment(t *testing.T) {
	got := ExtractBashCommands("FOO=bar git status")
	want := []string{"git"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_SubshellSurvivesAsOneAtomicWord(t *testing.T) {
	got := ExtractBashCommands(`FROM=$(date -v-6d +%F || date -d '6 days ago' +%F) && echo "$FROM"`)
	want := []string{"echo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_HeredocBodyDoesNotLeakSegments(t *testing.T) {
	got := ExtractBashCommands("cat <<EOF\nif this; then\n  echo leaked\nfi\nEOF\npwd")
	want := []string{"cat", "pwd"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_IndentedHeredocDelimiter(t *testing.T) {
	got := ExtractBashCommands("cat <<-EOF\n\ttext\n\tEOF\npwd")
	want := []string{"cat", "pwd"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_UnterminatedHeredocFallsThroughAsText(t *testing.T) {
	// heredocEnd returns -1 (no closing line), so the remainder is scanned
	// as ORDINARY text, not swallowed: it still segments on the embedded
	// newline, and "some" (a plausible-looking word) is picked up as a
	// second, spurious candidate. This is the real, faithfully-ported
	// behavior — "falls through as ordinary text" means "gets tokenized
	// normally," not "produces no extra segments." Confirmed by running
	// this against the implementation, not assumed.
	got := ExtractBashCommands("cat <<EOF\nsome text with no closer")
	want := []string{"cat", "some"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_LineContinuationJoinsWithoutBreak(t *testing.T) {
	got := ExtractBashCommands("git \\\n  status")
	want := []string{"git"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_RtkUnwrapsToRealCommand(t *testing.T) {
	got := ExtractBashCommands("rtk git status")
	want := []string{"git"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_RejectsShellKeywords(t *testing.T) {
	// Only the FIRST word of each ;-split segment is ever considered as a
	// candidate — there's no fallback to a later word in the same segment.
	// "do" is segment 2's first word and is rejected as a keyword, so
	// "echo" (same segment) never gets examined at all; the whole line
	// yields nothing. Confirmed by running this against the
	// implementation, not assumed — matches the JS's `let name =
	// tokens[i]` picking exactly one candidate per segment.
	got := ExtractBashCommands("for f in *; do echo $f; done")
	if len(got) != 0 {
		t.Errorf("got %v, want none (keyword rejection drops the whole segment)", got)
	}
}

func TestExtractBashCommands_RejectsNamesWithDisallowedChars(t *testing.T) {
	got := ExtractBashCommands(`$(echo hidden)`)
	if len(got) != 0 {
		t.Errorf("got %v, want none (name starts with disallowed char)", got)
	}
}

func TestExtractBashCommands_EmptyInput(t *testing.T) {
	if got := ExtractBashCommands(""); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}
