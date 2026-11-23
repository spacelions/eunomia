package caserun

import (
	"errors"
	"slices"
	"strings"

	"github.com/oklog/ulid/v2"
)

// Status represents a CaseRun's lifecycle state.
type Status string

const (
	StatusPending         Status = "pending"
	StatusRunning         Status = "running"
	StatusWaitingHuman    Status = "waiting_human"
	StatusWaitingApproval Status = "waiting_approval"
	StatusCompleted       Status = "completed"
	StatusEscalated       Status = "escalated"
	StatusFailed          Status = "failed"
	StatusCancelled       Status = "cancelled"
)

var (
	// ErrRunNotFound is returned when a CaseRun does not exist for the tenant.
	ErrRunNotFound = errors.New("case run not found")

	// ErrInvalidTransition is returned when a status transition violates the CaseRun state machine
	// or loses an optimistic concurrency race against another status update.
	ErrInvalidTransition = errors.New("invalid case run status transition")
)

var allowedTransitions = map[Status][]Status{
	StatusPending: {
		StatusRunning,
		StatusFailed,
		StatusCancelled,
	},
	StatusRunning: {
		StatusWaitingHuman,
		StatusWaitingApproval,
		StatusCompleted,
		StatusEscalated,
		StatusFailed,
		StatusCancelled,
	},
	StatusWaitingHuman: {
		StatusRunning,
		StatusEscalated,
		StatusFailed,
		StatusCancelled,
	},
	StatusWaitingApproval: {
		StatusRunning,
		StatusEscalated,
		StatusFailed,
		StatusCancelled,
	},
}

// NewRunID generates a time-sortable, lowercase Stripe-style run identifier (run_<ulid>).
func NewRunID() string {
	return "run_" + strings.ToLower(ulid.Make().String())
}

// IsValid reports whether s is a recognized CaseRun status.
func (s Status) IsValid() bool {
	switch s {
	case StatusPending, StatusRunning, StatusWaitingHuman, StatusWaitingApproval,
		StatusCompleted, StatusEscalated, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether s is a final state with no outgoing transitions.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusEscalated, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// CanTransition reports whether moving directly from -> to is permitted by the state machine.
func CanTransition(from, to Status) bool {
	return slices.Contains(allowedTransitions[from], to)
}
