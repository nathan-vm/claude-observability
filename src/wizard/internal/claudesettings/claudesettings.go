// Package claudesettings merges environment variables into the "env" object
// of a Claude Code settings.json, leaving every other key untouched.
package claudesettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"claude-observability-wizard/internal/envwriter"
)

// Overwrite records a managed key whose existing value was replaced.
type Overwrite struct {
	Name string
	Old  string
	New  string
}

type member struct {
	key string
	val json.RawMessage
}

// MergeEnv sets each var as a string in the "env" object of the settings file
// at path, creating the file (and its directory) if needed. Existing managed
// keys are overwritten; everything else, including key order, is preserved.
// A symlinked path is written through to its target. The file is left
// untouched, and changed is false, when every var already holds the requested
// value. overwritten lists the managed keys that already held a different
// value. Invalid JSON or a non-object top level / "env" is an error and
// never modifies the file.
func MergeEnv(path string, vars []envwriter.Var) (changed bool, overwritten []Overwrite, err error) {
	target := path
	if _, statErr := os.Lstat(path); statErr == nil {
		target, err = filepath.EvalSymlinks(path)
		if err != nil {
			return false, nil, err
		}
	}

	data, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		return false, nil, err
	}
	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(target); statErr == nil {
		mode = info.Mode().Perm()
	}

	out, changed, overwritten, err := mergeEnv(data, vars)
	if err != nil || !changed {
		return false, nil, err
	}

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, nil, err
	}
	if err := writeAtomic(dir, target, out, mode); err != nil {
		return false, nil, err
	}
	return true, overwritten, nil
}

func writeAtomic(dir, target string, data []byte, mode os.FileMode) (err error) {
	tmp, err := os.CreateTemp(dir, ".settings.json.*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

func mergeEnv(data []byte, vars []envwriter.Var) (out []byte, changed bool, overwritten []Overwrite, err error) {
	var top []member
	if len(bytes.TrimSpace(data)) > 0 {
		if top, err = parseObject(data); err != nil {
			return nil, false, nil, err
		}
	}

	envIdx := -1
	for i, m := range top {
		if m.key == "env" {
			envIdx = i
			break
		}
	}

	var env []member
	if envIdx >= 0 {
		if env, err = parseObject(top[envIdx].val); err != nil {
			return nil, false, nil, fmt.Errorf(`"env" is not a JSON object: %w`, err)
		}
	}

	for _, v := range vars {
		raw, err := marshalString(v.Value)
		if err != nil {
			return nil, false, nil, err
		}
		idx := -1
		for i, m := range env {
			if m.key == v.Name {
				idx = i
				break
			}
		}
		switch {
		case idx < 0:
			env = append(env, member{v.Name, raw})
			changed = true
		case !sameString(env[idx].val, v.Value):
			overwritten = append(overwritten, Overwrite{Name: v.Name, Old: displayValue(env[idx].val), New: v.Value})
			env[idx].val = raw
			changed = true
		}
	}
	if !changed {
		return nil, false, nil, nil
	}

	envRaw, err := encodeObject(env)
	if err != nil {
		return nil, false, nil, err
	}
	if envIdx >= 0 {
		top[envIdx].val = envRaw
	} else {
		top = append(top, member{"env", envRaw})
	}
	compact, err := encodeObject(top)
	if err != nil {
		return nil, false, nil, err
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, "", "  "); err != nil {
		return nil, false, nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), true, overwritten, nil
}

func parseObject(data []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	members := []member{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		members = append(members, member{keyTok.(string), val})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after JSON object")
	}
	return members, nil
}

func encodeObject(members []member) (json.RawMessage, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := marshalString(m.key)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshalString(s string) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return json.RawMessage(strings.TrimRight(b.String(), "\n")), nil
}

func sameString(raw json.RawMessage, want string) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && s == want
}

func displayValue(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}
