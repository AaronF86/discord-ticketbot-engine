package core

import (
	"slices"
	"time"
)

type TicketID int64

type Status string

const (
	StatusOpen         Status = "open"
	StatusClosed       Status = "closed"
	StatusArchived     Status = "archived"
	StatusPendingClose Status = "pending_close"
)

type Ticket struct {
	ID               TicketID
	PanelID          string
	Status           Status
	CreatedAt        time.Time
	LastMessage      time.Time
	MessageCount     int
	LeftOnRead       bool
	CreatedBy        string
	ClaimedBy        string
	CloseRequestedBy string
	Users            []string
	Meta             map[string]string
	Version          int64
	WorkflowState    WorkflowState

	FirstStaffReplyAt time.Time
	WaitStartedAt     time.Time
	TotalWaitSecs     int64
}

var transitions = map[Status][]Status{
	StatusOpen:         {StatusPendingClose, StatusClosed, StatusArchived},
	StatusPendingClose: {StatusOpen, StatusClosed, StatusArchived},
	StatusClosed:       {StatusArchived},
	StatusArchived:     {},
}

func (s Status) CanTransitionTo(next Status) bool {
	return slices.Contains(transitions[s], next)
}

func (s Status) Live() bool { return s == StatusOpen || s == StatusPendingClose }

func (t *Ticket) moveTo(next Status) error {
	if !t.Status.CanTransitionTo(next) {
		return ErrBadStatus
	}
	t.Status = next
	return nil
}
