// Package state persists dash-generator's small rate-publish dedup
// record between runs.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// State is dash-generator's entire persisted state: the last-published
// timestamp per "halflife:email" key, so a restart never re-publishes or
// duplicates a rate point.
type State struct {
	RatePublished map[string]int64 `json:"ratePublished"`
}

// Load reads path, or returns a zero-value State if it doesn't exist yet.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{RatePublished: map[string]int64{}}, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st.RatePublished == nil {
		st.RatePublished = map[string]int64{}
	}
	return &st, nil
}

// Save writes st to path, creating parent directories as needed, via a
// write-temp-then-rename so a kill mid-write never leaves truncated state.
func Save(path string, st *State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
