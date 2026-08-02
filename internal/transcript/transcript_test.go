package transcript

import (
	"strings"
	"testing"

	"my-agent-core/types"
)

func TestToMessages(t *testing.T) {
	raw := `[
		{"role": "system", "content": "you are a helper"},
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": "hi there"},
		{"role": "tool", "content": "result payload"},
		{"role": "user", "content": ""}
	]`
	msgs, err := ToMessages(raw)
	if err != nil {
		t.Fatalf("ToMessages error: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (system + empty dropped)", len(msgs))
	}
	if msgs[0].Role != types.RoleUser || msgs[0].Text != "hello" {
		t.Errorf("msg[0] = %+v", msgs[0])
	}
	if msgs[1].Role != types.RoleAssistant || msgs[1].Text != "hi there" {
		t.Errorf("msg[1] = %+v", msgs[1])
	}
	if msgs[2].Role != types.RoleToolResult {
		t.Errorf("msg[2] role = %v, want toolResult", msgs[2].Role)
	}
}

func TestToMessagesEmpty(t *testing.T) {
	for _, raw := range []string{"", "  ", "[]", "[{}]"} {
		msgs, err := ToMessages(raw)
		if err != nil {
			t.Fatalf("ToMessages(%q) error: %v", raw, err)
		}
		if len(msgs) != 0 {
			t.Errorf("ToMessages(%q) = %d messages, want 0", raw, len(msgs))
		}
	}
}

func TestToMessagesInvalid(t *testing.T) {
	if _, err := ToMessages("{not json"); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestCompressReusesReversible(t *testing.T) {
	// A large tool-result message should be truncated by ReversibleCompress.
	big := strings.Repeat("x", 500)
	msgs := []types.Message{
		{Role: types.RoleToolResult, ToolName: "read", Text: big},
		{Role: types.RoleUser, Text: "keep me"},
	}
	got := Compress(msgs)
	if len(got) != 2 {
		t.Fatalf("compressed = %d, want 2", len(got))
	}
	if len(got[0].Text) > 200 {
		t.Errorf("tool-result not compressed (len=%d)", len(got[0].Text))
	}
	if got[1].Text != "keep me" {
		t.Errorf("user message altered: %q", got[1].Text)
	}
}
