package execution

import (
	"database/sql"
	"errors"
	"time"
)

type State string

const (
	Queued    State = "queued"
	Running   State = "running"
	Completed State = "completed"
	Failed    State = "failed"
	Uncertain State = "uncertain"
)

var (
	ErrNotFound = errors.New("execution not found")
	ErrConflict = errors.New("idempotency payload conflict")
	ErrBlocked  = errors.New("session blocked by uncertain execution")
	ErrLimit    = errors.New("execution quota exceeded")
	ErrStale    = errors.New("execution ownership lost")
	ErrNotHead  = errors.New("execution is not session head")
	ErrBusy     = errors.New("execution is already running")
)

type Limits struct {
	SessionQueue int
	UserQueue    int
	AcceptRPM    int
	TaskTimeout  time.Duration
}

type Repository struct {
	DB     *sql.DB
	limits Limits
}

func NewRepository(db *sql.DB, limits Limits) *Repository {
	if limits.SessionQueue <= 0 {
		limits.SessionQueue = 8
	}
	if limits.UserQueue <= 0 {
		limits.UserQueue = 32
	}
	if limits.AcceptRPM <= 0 {
		limits.AcceptRPM = 60
	}
	if limits.TaskTimeout <= 0 {
		limits.TaskTimeout = 5 * time.Minute
	}
	return &Repository{DB: db, limits: limits}
}

type Input struct {
	UserID, SessionID                                                     int64
	ClientID, PayloadHash, Content, Provider, Model, Snapshot, QuotaScope string
	AttachmentIDs                                                         []int64
}

type Receipt struct {
	ID                 string `json:"request_id"`
	ClientID           string `json:"client_id"`
	SessionID          int64  `json:"session_id"`
	UserMessageID      int64  `json:"user_message_id"`
	AssistantMessageID int64  `json:"assistant_message_id,omitempty"`
	Sequence           int64  `json:"sequence"`
	State              State  `json:"state"`
	ErrorCode          string `json:"error_code,omitempty"`
}

type Evidence struct {
	Receipt
	UserID                                                    int64
	PayloadHash, Provider, Model, Snapshot, QuotaScope, Owner string
	Fence, LeaseUntilMS, DeadlineMS                           int64
	CallStarted                                               bool
	AttachmentIDs                                             []int64
}

type Delivery struct {
	ID         string
	Generation int64
}
type QuotaLimit struct {
	Concurrency int
	RPM         int
	TPM         int64
}
type QuotaPolicy struct {
	Platform, User, Credential QuotaLimit
	Grace                      time.Duration
}

type Attachment struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"-"`
	SessionID int64  `json:"session_id"`
	Name      string `json:"file_name"`
	MIME      string `json:"mime_type"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Data      []byte `json:"-"`
}
