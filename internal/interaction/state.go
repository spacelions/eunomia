package interaction

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/oklog/ulid/v2"
)

// Kind distinguishes an open-ended human question (ask_human) from a binary/draft approval (request_approval).
type Kind string

const (
	KindQuestion Kind = "question"
	KindApproval Kind = "approval"
)

// Status represents an InteractionTask's lifecycle state.
type Status string

const (
	StatusOpen      Status = "open"
	StatusSubmitted Status = "submitted"
	StatusApproved  Status = "approved"
	StatusRejected  Status = "rejected"
	StatusExpired   Status = "expired"
	StatusCancelled Status = "cancelled"
)

var (
	// ErrTaskNotFound is returned when an interaction task or Magic Link token does not exist.
	ErrTaskNotFound = errors.New("interaction task not found")

	// ErrInvalidTransition is returned when a status transition is disallowed for the task's kind
	// or loses an optimistic concurrency race against another resolution (e.g. double-approve).
	ErrInvalidTransition = errors.New("invalid interaction task transition")
)

var allowedByKind = map[Kind][]Status{
	KindQuestion: {StatusSubmitted, StatusExpired, StatusCancelled},
	KindApproval: {StatusApproved, StatusRejected, StatusExpired, StatusCancelled},
}

// NewTaskID generates a time-sortable, lowercase Stripe-style task identifier (task_<ulid>).
func NewTaskID() string {
	return "task_" + strings.ToLower(ulid.Make().String())
}

// IsValid reports whether k is a supported interaction kind.
func (k Kind) IsValid() bool {
	return k == KindQuestion || k == KindApproval
}

// IsTerminal reports whether s is a final state (everything other than StatusOpen).
func (s Status) IsTerminal() bool {
	switch s {
	case StatusSubmitted, StatusApproved, StatusRejected, StatusExpired, StatusCancelled:
		return true
	default:
		return false
	}
}

// CanTransition reports whether a task of `kind` may move from `from` -> `to`.
func CanTransition(kind Kind, from, to Status) bool {
	if from != StatusOpen {
		return false
	}
	return slices.Contains(allowedByKind[kind], to)
}

// GenerateToken produces a 32-byte cryptographically random URL-safe Magic Link token
// along with its SHA-256 hex digest. Only the hash is stored in PostgreSQL.
func GenerateToken() (plaintext string, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	plaintext = base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashToken(plaintext), nil
}

// HashToken returns the canonical SHA-256 hex digest of a raw Magic Link token.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
