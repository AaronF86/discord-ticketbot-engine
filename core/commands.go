package core

import (
	"context"
	"errors"
	"slices"
	"time"
)

var (
	ErrNotFound      = errors.New("ticket not found")
	ErrForbidden     = errors.New("permission denied")
	ErrBadStatus     = errors.New("ticket is in an invalid state for this action")
	ErrAutoConfirmed = errors.New("close request auto-confirmed")
)

type Hooks struct {
	AfterOpen         func(ctx context.Context, t *Ticket) error
	BeforeClose       func(ctx context.Context, actor string, id TicketID) error
	AfterClose        func(ctx context.Context, actor string, id TicketID) error
	AfterForceClose   func(ctx context.Context, actor string, id TicketID) error
	AfterAddUser      func(ctx context.Context, actor string, id TicketID, userID string) error
	AfterRemoveUser   func(ctx context.Context, actor string, id TicketID, userID string) error
	AfterClaim        func(ctx context.Context, actor string, id TicketID) error
	AfterUnclaim      func(ctx context.Context, actor string, id TicketID) error
	AfterSetStatus    func(ctx context.Context, actor string, id TicketID, status Status) error
	AfterRequestClose func(ctx context.Context, actor string, id TicketID) error
	AfterConfirmClose func(ctx context.Context, actor string, id TicketID) error
	AfterSetMetrics   func(ctx context.Context, actor string, id TicketID, metrics map[string]any) error
	AfterSetMeta      func(ctx context.Context, actor string, id TicketID, meta map[string]string) error
}

type Service struct {
	store            Store
	auth             Authorizer
	panels           *PanelRegistry
	hooks            Hooks
	GlobalMaxTickets int
}

func NewService(store Store, auth Authorizer, panels *PanelRegistry, hooks Hooks) *Service {
	return &Service{store: store, auth: auth, panels: panels, hooks: hooks}
}

func (s *Service) Auth() Authorizer { return s.auth }

func (s *Service) allow(ctx context.Context, actor string, action Action, id TicketID) error {
	ok, err := s.auth.Allow(ctx, actor, action, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

var ErrTicketLimitReached = errors.New("ticket limit reached")

func (s *Service) Open(ctx context.Context, actor, panelID string) (*Ticket, error) {
	panel, ok := s.panels.Get(panelID)
	if !ok {
		return nil, errors.New("unknown panel")
	}

	limit := s.GlobalMaxTickets
	if panel.MaxTickets > 0 {
		limit = panel.MaxTickets
	}
	dbPanel, dbGlobal, _ := s.store.PanelLimits(ctx, panelID)
	if dbGlobal > 0 {
		limit = dbGlobal
	}
	if dbPanel > 0 {
		limit = dbPanel
	}
	if limit > 0 {
		existing, err := s.store.ListByUser(ctx, actor)
		if err != nil {
			return nil, err
		}
		open := 0
		for _, t := range existing {
			if t.PanelID == panelID && t.Status == StatusOpen {
				open++
			}
		}
		if open >= limit {
			return nil, ErrTicketLimitReached
		}
	}

	t := &Ticket{
		PanelID:   panelID,
		Status:    StatusOpen,
		CreatedBy: actor,
		Users:     []string{actor},
	}

	if err := s.store.Create(ctx, t); err != nil {
		return nil, err
	}

	t.WaitStartedAt = t.CreatedAt

	if panel.Workflow != nil {
		if err := panel.Workflow.Execute(ctx, t); err != nil {
			return nil, err
		}
		if err := s.store.Update(ctx, t); err != nil {
			return nil, err
		}
	}

	if s.hooks.AfterOpen != nil {
		if err := s.hooks.AfterOpen(ctx, t); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (s *Service) Close(ctx context.Context, actor string, id TicketID) error {
	if err := s.allow(ctx, actor, ActionClose, id); err != nil {
		return err
	}
	if s.hooks.BeforeClose != nil {
		if err := s.hooks.BeforeClose(ctx, actor, id); err != nil {
			return err
		}
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != StatusOpen {
		return ErrBadStatus
	}
	now := time.Now().UTC()
	t.Status = StatusClosed
	if err := s.store.Update(ctx, t); err != nil {
		return err
	}
	if err := s.finalizeMetrics(ctx, t, now); err != nil {
		return err
	}
	if s.hooks.AfterClose != nil {
		return s.hooks.AfterClose(ctx, actor, id)
	}
	return nil
}

func (s *Service) ForceClose(ctx context.Context, actor string, id TicketID) error {
	if err := s.allow(ctx, actor, ActionForceClose, id); err != nil {
		return err
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	t.Status = StatusClosed
	if err := s.store.Update(ctx, t); err != nil {
		return err
	}
	if err := s.finalizeMetrics(ctx, t, now); err != nil {
		return err
	}
	if s.hooks.AfterForceClose != nil {
		return s.hooks.AfterForceClose(ctx, actor, id)
	}
	return nil
}

func (s *Service) AddUser(ctx context.Context, actor string, id TicketID, userID string) error {
	if err := s.allow(ctx, actor, ActionAddUser, id); err != nil {
		return err
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	for _, u := range t.Users {
		if u == userID {
			return nil
		}
	}
	t.Users = append(t.Users, userID)
	if err := s.store.Update(ctx, t); err != nil {
		return err
	}
	if s.hooks.AfterAddUser != nil {
		return s.hooks.AfterAddUser(ctx, actor, id, userID)
	}
	return nil
}

func (s *Service) RemoveUser(ctx context.Context, actor string, id TicketID, userID string) error {
	if err := s.allow(ctx, actor, ActionRemoveUser, id); err != nil {
		return err
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	filtered := t.Users[:0]
	for _, u := range t.Users {
		if u != userID {
			filtered = append(filtered, u)
		}
	}
	t.Users = filtered
	if err := s.store.Update(ctx, t); err != nil {
		return err
	}
	if s.hooks.AfterRemoveUser != nil {
		return s.hooks.AfterRemoveUser(ctx, actor, id, userID)
	}
	return nil
}

func (s *Service) Claim(ctx context.Context, actor string, id TicketID) error {
	if err := s.allow(ctx, actor, ActionClaim, id); err != nil {
		return err
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	t.ClaimedBy = actor
	if err := s.store.Update(ctx, t); err != nil {
		return err
	}
	if s.hooks.AfterClaim != nil {
		return s.hooks.AfterClaim(ctx, actor, id)
	}
	return nil
}

func (s *Service) Unclaim(ctx context.Context, actor string, id TicketID) error {
	if err := s.allow(ctx, actor, ActionUnclaim, id); err != nil {
		return err
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	t.ClaimedBy = ""
	if err := s.store.Update(ctx, t); err != nil {
		return err
	}
	if s.hooks.AfterUnclaim != nil {
		return s.hooks.AfterUnclaim(ctx, actor, id)
	}
	return nil
}

func (s *Service) SetStatus(ctx context.Context, actor string, id TicketID, status Status) error {
	if err := s.allow(ctx, actor, ActionSetStatus, id); err != nil {
		return err
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	t.Status = status
	if err := s.store.Update(ctx, t); err != nil {
		return err
	}
	if s.hooks.AfterSetStatus != nil {
		return s.hooks.AfterSetStatus(ctx, actor, id, status)
	}
	return nil
}

func (s *Service) CancelPendingClose(ctx context.Context, id TicketID) error {
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != StatusPendingClose {
		return ErrBadStatus
	}
	t.Status = StatusOpen
	return s.store.Update(ctx, t)
}

func (s *Service) RecordMessage(ctx context.Context, id TicketID, actor string) error {
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.Status == StatusClosed || t.Status == StatusArchived {
		return nil
	}
	now := time.Now().UTC()
	t.LastMessage = now

	if slices.Contains(t.Users, actor) {
		if t.WaitStartedAt.IsZero() {
			t.WaitStartedAt = now
		}
		t.LeftOnRead = true
	} else {
		if t.FirstStaffReplyAt.IsZero() {
			t.FirstStaffReplyAt = now
		}
		if !t.WaitStartedAt.IsZero() {
			t.TotalWaitSecs += int64(now.Sub(t.WaitStartedAt).Seconds())
			t.WaitStartedAt = time.Time{}
		}
		t.LeftOnRead = false
	}

	if err := s.store.Update(ctx, t); err != nil {
		return err
	}
	return s.store.IncrementMetric(ctx, id, "message_count")
}

func (s *Service) finalizeMetrics(ctx context.Context, t *Ticket, now time.Time) error {
	if err := s.store.UpsertMetric(ctx, t.ID, "time_to_close", now.Sub(t.CreatedAt)); err != nil {
		return err
	}
	if !t.FirstStaffReplyAt.IsZero() {
		if err := s.store.UpsertMetric(ctx, t.ID, "time_to_first_reply", t.FirstStaffReplyAt.Sub(t.CreatedAt)); err != nil {
			return err
		}
	}
	totalWait := time.Duration(t.TotalWaitSecs) * time.Second
	if !t.WaitStartedAt.IsZero() {
		totalWait += now.Sub(t.WaitStartedAt)
	}
	return s.store.UpsertMetric(ctx, t.ID, "total_wait_time", totalWait)
}

func (s *Service) SetMeta(ctx context.Context, actor string, id TicketID, meta map[string]string) error {
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.Meta == nil {
		t.Meta = make(map[string]string)
	}
	for k, v := range meta {
		t.Meta[k] = v
	}
	if err := s.store.Update(ctx, t); err != nil {
		return err
	}
	if s.hooks.AfterSetMeta != nil {
		return s.hooks.AfterSetMeta(ctx, actor, id, meta)
	}
	return nil
}

func (s *Service) SetMetrics(ctx context.Context, actor string, id TicketID, metrics map[string]any) error {
	for k, v := range metrics {
		if err := s.store.UpsertMetric(ctx, id, k, v); err != nil {
			return err
		}
	}
	if s.hooks.AfterSetMetrics != nil {
		return s.hooks.AfterSetMetrics(ctx, actor, id, metrics)
	}
	return nil
}

func (s *Service) RequestClose(ctx context.Context, actor string, id TicketID) error {
	if err := s.allow(ctx, actor, ActionRequestClose, id); err != nil {
		return err
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}

	if t.Status == StatusPendingClose && t.CloseRequestedBy != "" && t.CloseRequestedBy != actor {
		now := time.Now().UTC()
		t.Status = StatusClosed
		t.CloseRequestedBy = ""
		if err := s.store.Update(ctx, t); err != nil {
			return err
		}
		if err := s.finalizeMetrics(ctx, t, now); err != nil {
			return err
		}
		if s.hooks.AfterConfirmClose != nil {
			if err := s.hooks.AfterConfirmClose(ctx, actor, id); err != nil {
				return err
			}
		}
		return ErrAutoConfirmed
	}
	if t.Status != StatusOpen {
		return ErrBadStatus
	}
	t.Status = StatusPendingClose
	t.CloseRequestedBy = actor
	if err := s.store.Update(ctx, t); err != nil {
		return err
	}
	if s.hooks.AfterRequestClose != nil {
		return s.hooks.AfterRequestClose(ctx, actor, id)
	}
	return nil
}

func (s *Service) ConfirmClose(ctx context.Context, actor string, id TicketID) error {
	if err := s.allow(ctx, actor, ActionConfirmClose, id); err != nil {
		return err
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != StatusPendingClose {
		return ErrBadStatus
	}
	now := time.Now().UTC()
	t.Status = StatusClosed
	t.CloseRequestedBy = ""
	if err := s.store.Update(ctx, t); err != nil {
		return err
	}
	if err := s.finalizeMetrics(ctx, t, now); err != nil {
		return err
	}
	if s.hooks.AfterConfirmClose != nil {
		return s.hooks.AfterConfirmClose(ctx, actor, id)
	}
	return nil
}

func (s *Service) Get(ctx context.Context, id TicketID) (*Ticket, error) {
	return s.store.Get(ctx, id)
}

func (s *Service) Archive(ctx context.Context, id TicketID) error {
	return s.store.Archive(ctx, id)
}

func (s *Service) ListByUser(ctx context.Context, userID string) ([]*Ticket, error) {
	return s.store.ListByUser(ctx, userID)
}

func (s *Service) AllPanels() []*Panel {
	return s.panels.All()
}

func (s *Service) Panel(id string) (*Panel, bool) {
	return s.panels.Get(id)
}
