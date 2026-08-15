package httpapi

import "time"

// sessionRequest is the POST /api/v1/{agent}/sessions body.
type sessionRequest struct {
	SessionID string `json:"session_id"`
	TurnCount int    `json:"turn_count"`
	RawTurns  string `json:"raw_turns"` // JSON: [{role, content}]
	// SessionDate is the session start time passed through by the client
	// (RFC3339). Optional — when invalid or missing the field stays nil.
	SessionDate string `json:"session_date,omitempty"`
	// KanbanBoard identifies the kanban board this session belongs to
	// (board slug/ID, e.g. "default"), set by the Hermes bridge when the
	// session was a dispatcher-spawned kanban worker. Optional — absent for
	// ordinary sessions. When present and matching interestmemory.kanban_exclude,
	// the transcript is skipped entirely (not stored, not processed, no tokens).
	KanbanBoard string `json:"kanban_board,omitempty"`
	// KanbanBoardName is the board's display name (best-effort from
	// board.json, may be empty). Matched case-insensitively against
	// interestmemory.kanban_exclude alongside KanbanBoard.
	KanbanBoardName string `json:"kanban_board_name,omitempty"`
}

func timeNow() time.Time { return time.Now().UTC() }
