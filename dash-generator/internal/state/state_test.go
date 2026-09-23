package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_MissingFileReturnsZeroValue(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.RatePublished == nil || len(st.RatePublished) != 0 {
		t.Errorf("RatePublished = %v, want empty non-nil map", st.RatePublished)
	}
}

func TestSaveThenLoad_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := &State{RatePublished: map[string]int64{"20m:a@example.com": 1700000000}}
	if err := Save(path, st); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RatePublished["20m:a@example.com"] != 1700000000 {
		t.Errorf("loaded = %v", loaded.RatePublished)
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "state.json")
	if err := Save(path, &State{RatePublished: map[string]int64{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
