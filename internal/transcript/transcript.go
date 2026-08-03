package transcript

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/djasdh/my-agent-core/types"
	"github.com/djasdh/my-agent-core/wiki"
)

// rawTurn is the Hermes wire format for a single transcript message:
// [{role, content}]. role is "user" | "assistant" | "system" | "tool".
type rawTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ToMessages converts a Hermes transcript (JSON array of {role, content})
// into my-agent-core []types.Message. System and empty messages are
// dropped; tool-result messages carry their content in Text (no ToolName).
func ToMessages(raw string) ([]types.Message, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var turns []rawTurn
	if err := json.Unmarshal([]byte(raw), &turns); err != nil {
		return nil, fmt.Errorf("transcript: parse: %w", err)
	}
	out := make([]types.Message, 0, len(turns))
	for _, t := range turns {
		content := strings.TrimSpace(t.Content)
		if content == "" {
			continue
		}
		var role types.Role
		switch strings.ToLower(t.Role) {
		case "user":
			role = types.RoleUser
		case "assistant":
			role = types.RoleAssistant
		case "tool", "toolresult", "tool_result":
			role = types.RoleToolResult
		default: // system and unknown roles are not interest-bearing
			continue
		}
		out = append(out, types.Message{Role: role, Text: content})
	}
	return out, nil
}

// Compress wraps my-agent-core wiki.ReversibleCompress for transcript
// messages (best-effort token reduction before feeding the wiki agent).
func Compress(msgs []types.Message) []types.Message {
	return wiki.ReversibleCompress(msgs)
}
