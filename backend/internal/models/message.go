package models

import "time"

// Message captures an individual user input stored in the history.

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

type Message struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	SessionID int64     `json:"session_id"`
	Role      Role      `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}
