package limits

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdate_CreatesFromExampleIfMissing(t *testing.T) {
	dir := t.TempDir()
	example := filepath.Join(dir, "account-limits.example.json")
	os.WriteFile(example, []byte(`{"default":{"block_5h":1},"accounts":{"x@example.com":{}},"ignore":[]}`), 0o644)
	path := filepath.Join(dir, "account-limits.json")

	if err := Update(path, example, []string{"a@example.com"}); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	ignore := doc["ignore"].([]interface{})
	if len(ignore) != 1 || ignore[0] != "a@example.com" {
		t.Errorf("ignore = %v, want [a@example.com]", ignore)
	}
	if _, ok := doc["default"]; !ok {
		t.Error("default field not preserved")
	}
	accounts := doc["accounts"].(map[string]interface{})
	if _, ok := accounts["x@example.com"]; !ok {
		t.Errorf("accounts = %v, want untouched from example (not cleared)", accounts)
	}
}

func TestUpdate_DedupesAndSkipsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account-limits.json")
	os.WriteFile(path, []byte(`{"ignore":[],"accounts":{}}`), 0o644)

	err := Update(path, filepath.Join(dir, "unused-example.json"), []string{"a@example.com", "a@example.com", ""})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var doc map[string]interface{}
	json.Unmarshal(data, &doc)
	ignore := doc["ignore"].([]interface{})
	if len(ignore) != 1 {
		t.Errorf("ignore = %v, want 1 deduped entry", ignore)
	}
}

func TestUpdate_PreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account-limits.json")
	os.WriteFile(path, []byte(`{"_comment":["hi"],"default":{"block_5h":99},"accounts":{"old@example.com":{}},"ignore":["old@example.com"]}`), 0o644)

	if err := Update(path, filepath.Join(dir, "unused.json"), []string{"new@example.com"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var doc map[string]interface{}
	json.Unmarshal(data, &doc)
	if _, ok := doc["_comment"]; !ok {
		t.Error("_comment not preserved")
	}
	def := doc["default"].(map[string]interface{})
	if def["block_5h"].(float64) != 99 {
		t.Error("default.block_5h not preserved")
	}
	accounts := doc["accounts"].(map[string]interface{})
	if _, ok := accounts["old@example.com"]; !ok {
		t.Errorf("accounts = %v, want untouched (old-shape field left alone)", accounts)
	}
}
