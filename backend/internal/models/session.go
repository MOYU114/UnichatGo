package models

import "time"

// Session groups a sequence of user inputs.
type Session struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SessionChan is a helper channel type for streaming sessions.
type SessionChan chan *Session
