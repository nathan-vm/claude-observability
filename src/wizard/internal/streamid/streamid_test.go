package streamid

import (
	"regexp"
	"testing"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNew_LooksLikeUUIDv4(t *testing.T) {
	id, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if !uuidV4.MatchString(id) {
		t.Errorf("New() = %q, want a UUIDv4-shaped string", id)
	}
}

func TestNew_Unique(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("New() returned the same id twice: %q", a)
	}
}
