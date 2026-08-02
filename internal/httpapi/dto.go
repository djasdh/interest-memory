package httpapi

import "time"

// sessionRequest is the POST /api/v1/{agent}/sessions body.
type sessionRequest struct {
	SessionID string `json:"session_id"`
	TurnCount int    `json:"turn_count"`
	RawTurns  string `json:"raw_turns"` // JSON: [{role, content}]
}

func timeNow() time.Time { return time.Now().UTC() }
