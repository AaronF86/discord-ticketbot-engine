package core

import "time"

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
	WorkflowState    WorkflowState

	FirstStaffReplyAt time.Time
	WaitStartedAt     time.Time
	TotalWaitSecs     int64
}
