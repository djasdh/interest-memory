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
}

func timeNow() time.Time { return time.Now().UTC() }
