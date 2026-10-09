package core

import (
	"context"
	"errors"
)

var (
	ErrConflict = errors.New("ticket was modified concurrently")

	ErrTicketLimitReached = errors.New("ticket limit reached")
)

type Store interface {
	Create(ctx context.Context, t *Ticket, limit int) error
	Get(ctx context.Context, id TicketID) (*Ticket, error)
	Update(ctx context.Context, t *Ticket) error
	ListByPanel(ctx context.Context, panelID string) ([]*Ticket, error)
	ListByUser(ctx context.Context, userID string) ([]*Ticket, error)
	Archive(ctx context.Context, id TicketID) error

	UpsertMetric(ctx context.Context, id TicketID, key string, value any) error
	IncrementMetric(ctx context.Context, id TicketID, key string) error

	PanelLimits(ctx context.Context, panelID string) (panel int, global int, err error)
	SetPanelLimit(ctx context.Context, panelID string, maxTickets int) error
}
