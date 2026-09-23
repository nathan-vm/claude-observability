// Package state persists transcript-scan progress between passes: per-file
// byte offsets, in-flight tool-call groups, dedup, and skill-name
// resolution — port of transcript-scan.mjs's emptyState/loadState/saveState.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Group is one assistant message's worth of tool calls, still waiting to
// be settled by the NEXT assistant message's input-token usage.
type Group struct {
	SessionID   string `json:"sessionId"`
	RequestID   string `json:"requestId"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	GitBranch   string `json:"gitBranch"`
	Project     string `json:"project"`
	IsSidechain bool   `json:"isSidechain"`
	AtMs        int64  `json:"at"`
	Calls       []Call `json:"calls"`
}

// Call is one tool_use block within a Group.
type Call struct {
	ToolUseID    string   `json:"toolUseId"`
	ToolName     string   `json:"toolName"`
	ResultBytes  int      `json:"resultBytes"`
	Timestamp    string   `json:"timestamp"`
	BashCommands []string `json:"bashCommands,omitempty"`
	ToolSource   string   `json:"toolSource"`
	MCPServer    string   `json:"mcpServer"`
	MCPTool      string   `json:"mcpTool"`
}

// FileState is per-transcript-file progress: consumed byte offset, at
// most one open group per track (main/subagent), and each track's
// currently-active skill name (sticky until the next "Skill" tool call).
type FileState struct {
	Offset          int64  `json:"offset"`
	PendingMain     *Group `json:"pendingMain,omitempty"`
	PendingSub      *Group `json:"pendingSub,omitempty"`
	ActiveSkillMain string `json:"activeSkillMain"`
	ActiveSkillSub  string `json:"activeSkillSub"`
}

// PluginSkillRequest records which plugin skill was active at the moment
// of one specific request — the precise twin of the session-level
// PluginSkills set, needed when a session used 2+ plugin skills (the
// session-level set alone can't say which request used which).
type PluginSkillRequest struct {
	Skill string `json:"skill"`
	TsMs  int64  `json:"ts"`
}

// State is transcript-scan's entire persisted state.
type State struct {
	Version              int                           `json:"version"`
	Files                map[string]*FileState         `json:"files"`
	SessionEmail         map[string]string             `json:"sessionEmail"`
	Seen                 map[string]int64              `json:"seen"`
	PluginSkills         map[string][]string            `json:"pluginSkills"`
	PluginSkillByRequest map[string]PluginSkillRequest  `json:"pluginSkillByRequest"`
}

func empty() *State {
	return &State{
		Version:              2,
		Files:                map[string]*FileState{},
		SessionEmail:         map[string]string{},
		Seen:                 map[string]int64{},
		PluginSkills:         map[string][]string{},
		PluginSkillByRequest: map[string]PluginSkillRequest{},
	}
}

// Load reads path, or returns a fresh empty State if it doesn't exist yet.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return empty(), nil
		}
		return nil, err
	}
	st := empty()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	if st.Files == nil {
		st.Files = map[string]*FileState{}
	}
	if st.SessionEmail == nil {
		st.SessionEmail = map[string]string{}
	}
	if st.Seen == nil {
		st.Seen = map[string]int64{}
	}
	if st.PluginSkills == nil {
		st.PluginSkills = map[string][]string{}
	}
	if st.PluginSkillByRequest == nil {
		st.PluginSkillByRequest = map[string]PluginSkillRequest{}
	}
	st.Version = 2
	return st, nil
}

// Save writes st to path via write-temp-then-rename, so a kill mid-write
// never leaves truncated state that would make the next pass re-scan
// everything.
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
