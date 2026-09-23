// Package limits updates grafana/account-limits.json's "ignore" list — the
// accounts the setup wizard discovered but the user chose not to monitor.
package limits

import (
	"encoding/json"
	"os"
)

// Update sets the "ignore" field of the JSON file at path to ignored
// (deduped, skipping empty strings) and clears "accounts" to an empty
// object, preserving every other top-level field untouched. If path
// doesn't exist, it's first created as a copy of examplePath.
func Update(path, examplePath string, ignored []string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		data, err = os.ReadFile(examplePath)
		if err != nil {
			return err
		}
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}

	seen := make(map[string]bool, len(ignored))
	deduped := make([]interface{}, 0, len(ignored))
	for _, email := range ignored {
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		deduped = append(deduped, email)
	}
	doc["ignore"] = deduped
	doc["accounts"] = map[string]interface{}{}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}
