package models

import "time"

// TempFile represents a user-uploaded temporary document.
type TempFile struct {
	ID               int64     `json:"id"`
	UserID           int64     `json:"user_id"`
	SessionID        int64     `json:"session_id"`
	FileName         string    `json:"file_name"`
	StoredPath       string    `json:"stored_path"`
	MimeType         string    `json:"mime_type"`
	Size             int64     `json:"size"`
	Status           string    `json:"status"`
	Summary          string    `json:"summary"`
	SummaryMessageID int64     `json:"summary_message_id"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at"`
}
