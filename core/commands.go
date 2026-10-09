package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"
)

var (
	ErrNotFound  = errors.New("ticket not found")
	ErrForbidden = errors.New("permission denied")
	ErrBadStatus = errors.New("ticket is in an invalid state for this action")
)

type HookError struct {
	Hook string
	Err  error
}

func (e *HookError) Error() string {
	return fmt.Sprintf("%s hook failed (the change was already applied): %v", e.Hook, e.Err)
}

func (e *HookError) Unwrap() error { return e.Err }

func hookErr(name string, err error) error {
	if err == nil {
		return nil
	}
	return &HookError{Hook: name, Err: err}
}

type Hooks struct {
	AfterOpen         func(ctx context.Context, t *Ticket) error
	BeforeClose       func(ctx context.Context, actor string, id TicketID) error
	AfterClose        func(ctx context.Context, actor string, id TicketID) error
	BeforeForceClose  func(ctx context.Context, actor string, id TicketID) error
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

func NewService(store Store, auth Authorizer, panels *PanelRegistry, hooks *Hooks) *Service {
	s := &Service{store: store, auth: auth, panels: panels}
	if hooks != nil {
		s.hooks = *hooks
	}
	return s
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

const maxConflictRetries = 5

var errNoChange = errors.New("no change")

func (s *Service) mutate(ctx context.Context, id TicketID, fn func(t *Ticket) error) (*Ticket, error) {
	for attempt := 0; ; attempt++ {
		t, err := s.store.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := fn(t); err != nil {
			if errors.Is(err, errNoChange) {
				return t, nil
			}
			return nil, err
		}
		err = s.store.Update(ctx, t)
		if errors.Is(err, ErrConflict) && attempt < maxConflictRetries {
			continue
		}
		if err != nil {
			return nil, err
		}
		return t, nil
	}
}

type OpenOption func(*openConfig)

type openConfig struct {
	provision func(ctx context.Context, t *Ticket) error
}

func WithProvision(fn func(ctx context.Context, t *Ticket) error) OpenOption {
	return func(c *openConfig) { c.provision = fn }
}

func (s *Service) Open(ctx context.Context, actor, panelID string, opts ...OpenOption) (*Ticket, error) {
	var cfg openConfig
	for _, o := range opts {
		o(&cfg)
	}

	panel, ok := s.panels.Get(panelID)
	if !ok {
		return nil, errors.New("unknown panel")
	}

	limit, err := s.ticketLimit(ctx, panel)
	if err != nil {
		return nil, err
	}

	t := &Ticket{
		PanelID:   panelID,
		Status:    StatusOpen,
		CreatedBy: actor,
		Users:     []string{actor},
	}

	if err := s.store.Create(ctx, t, limit); err != nil {
		return nil, err
	}

	t.WaitStartedAt = t.CreatedAt

	if err := s.initTicket(ctx, panel, &cfg, t); err != nil {
		s.discard(ctx, t.ID)
		return nil, err
	}
	return t, nil
}

func (s *Service) ticketLimit(ctx context.Context, panel *Panel) (int, error) {
	limit := s.GlobalMaxTickets
	if panel.MaxTickets > 0 {
		limit = panel.MaxTickets
	}
	dbPanel, dbGlobal, err := s.store.PanelLimits(ctx, panel.ID)
	if err != nil {
		return 0, fmt.Errorf("load ticket limits: %w", err)
	}
	if dbGlobal > 0 {
		limit = dbGlobal
	}
	if dbPanel > 0 {
		limit = dbPanel
	}
	return limit, nil
}

func (s *Service) initTicket(ctx context.Context, panel *Panel, cfg *openConfig, t *Ticket) error {
	if panel.Workflow != nil {
		wfErr := panel.Workflow.Execute(ctx, t)
		if err := s.store.Update(ctx, t); err != nil && wfErr == nil {
			return err
		}
		if wfErr != nil {
			return wfErr
		}
	}

	if cfg.provision != nil {
		if err := cfg.provision(ctx, t); err != nil {
			return err
		}
	}

	if s.hooks.AfterOpen != nil {
		if err := s.hooks.AfterOpen(ctx, t); err != nil {
			return hookErr("AfterOpen", err)
		}
	}
	return nil
}

func (s *Service) discard(ctx context.Context, id TicketID) {
	if err := s.store.Archive(context.WithoutCancel(ctx), id); err != nil {
		slog.Error("discard failed open", "ticket", id, "err", err)
	}
}

func (s *Service) Close(ctx context.Context, actor string, id TicketID) error {
	if err := s.allow(ctx, actor, ActionClose, id); err != nil {
		return err
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != StatusOpen {
		return ErrBadStatus
	}
	if s.hooks.BeforeClose != nil {
		if err := s.hooks.BeforeClose(ctx, actor, id); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	t, err = s.mutate(ctx, id, func(t *Ticket) error {
		if t.Status != StatusOpen {
			return ErrBadStatus
		}
		return t.moveTo(StatusClosed)
	})
	if err != nil {
		return err
	}
	if err := s.finalizeMetrics(ctx, t, now); err != nil {
		return err
	}
	if s.hooks.AfterClose != nil {
		return hookErr("AfterClose", s.hooks.AfterClose(ctx, actor, id))
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
	if !t.Status.Live() {
		return ErrBadStatus
	}
	if s.hooks.BeforeForceClose != nil {
		if err := s.hooks.BeforeForceClose(ctx, actor, id); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	t, err = s.mutate(ctx, id, func(t *Ticket) error {
		t.CloseRequestedBy = ""
		return t.moveTo(StatusClosed)
	})
	if err != nil {
		return err
	}
	if err := s.finalizeMetrics(ctx, t, now); err != nil {
		return err
	}
	if s.hooks.AfterForceClose != nil {
		return hookErr("AfterForceClose", s.hooks.AfterForceClose(ctx, actor, id))
	}
	return nil
}

func (s *Service) AddUser(ctx context.Context, actor string, id TicketID, userID string) error {
	if err := s.allow(ctx, actor, ActionAddUser, id); err != nil {
		return err
	}
	if _, err := s.mutate(ctx, id, func(t *Ticket) error {
		if slices.Contains(t.Users, userID) {
			return errNoChange
		}
		t.Users = append(t.Users, userID)
		return nil
	}); err != nil {
		return err
	}
	if s.hooks.AfterAddUser != nil {
		return hookErr("AfterAddUser", s.hooks.AfterAddUser(ctx, actor, id, userID))
	}
	return nil
}

func (s *Service) RemoveUser(ctx context.Context, actor string, id TicketID, userID string) error {
	if err := s.allow(ctx, actor, ActionRemoveUser, id); err != nil {
		return err
	}
	if _, err := s.mutate(ctx, id, func(t *Ticket) error {
		t.Users = slices.DeleteFunc(slices.Clone(t.Users), func(u string) bool { return u == userID })
		return nil
	}); err != nil {
		return err
	}
	if s.hooks.AfterRemoveUser != nil {
		return hookErr("AfterRemoveUser", s.hooks.AfterRemoveUser(ctx, actor, id, userID))
	}
	return nil
}

func (s *Service) Claim(ctx context.Context, actor string, id TicketID) error {
	if err := s.allow(ctx, actor, ActionClaim, id); err != nil {
		return err
	}
	if _, err := s.mutate(ctx, id, func(t *Ticket) error {
		t.ClaimedBy = actor
		return nil
	}); err != nil {
		return err
	}
	if s.hooks.AfterClaim != nil {
		return hookErr("AfterClaim", s.hooks.AfterClaim(ctx, actor, id))
	}
	return nil
}

func (s *Service) Unclaim(ctx context.Context, actor string, id TicketID) error {
	if err := s.allow(ctx, actor, ActionUnclaim, id); err != nil {
		return err
	}
	if _, err := s.mutate(ctx, id, func(t *Ticket) error {
		t.ClaimedBy = ""
		return nil
	}); err != nil {
		return err
	}
	if s.hooks.AfterUnclaim != nil {
		return hookErr("AfterUnclaim", s.hooks.AfterUnclaim(ctx, actor, id))
	}
	return nil
}

func (s *Service) SetStatus(ctx context.Context, actor string, id TicketID, status Status) error {
	if err := s.allow(ctx, actor, ActionSetStatus, id); err != nil {
		return err
	}
	if status != StatusOpen {
		return ErrBadStatus
	}
	if _, err := s.mutate(ctx, id, func(t *Ticket) error {
		if t.Status == StatusOpen {
			return nil
		}
		t.CloseRequestedBy = ""
		return t.moveTo(StatusOpen)
	}); err != nil {
		return err
	}
	if s.hooks.AfterSetStatus != nil {
		return hookErr("AfterSetStatus", s.hooks.AfterSetStatus(ctx, actor, id, status))
	}
	return nil
}

func (s *Service) CancelPendingClose(ctx context.Context, id TicketID) error {
	_, err := s.mutate(ctx, id, func(t *Ticket) error {
		if t.Status != StatusPendingClose {
			return ErrBadStatus
		}
		t.CloseRequestedBy = ""
		return t.moveTo(StatusOpen)
	})
	return err
}

func (s *Service) RecordMessage(ctx context.Context, id TicketID, actor string) error {
	closed := false
	if _, err := s.mutate(ctx, id, func(t *Ticket) error {
		if !t.Status.Live() {
			closed = true
			return errNoChange
		}
		closed = false
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
		return nil
	}); err != nil {
		return err
	}
	if closed {
		return nil
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
	if _, err := s.mutate(ctx, id, func(t *Ticket) error {
		t.Meta = mergeMeta(t.Meta, meta)
		return nil
	}); err != nil {
		return err
	}
	if s.hooks.AfterSetMeta != nil {
		return hookErr("AfterSetMeta", s.hooks.AfterSetMeta(ctx, actor, id, meta))
	}
	return nil
}

func mergeMeta(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	maps.Copy(dst, src)
	return dst
}

func checkMetric(panel *Panel, key string, value any) error {
	schemas := BuiltinMetrics
	if panel != nil {
		schemas = append(slices.Clone(BuiltinMetrics), panel.Metrics...)
	}
	for _, m := range schemas {
		if m.Key == key && m.Kind != KindOf(value) {
			return fmt.Errorf("metric %q expects %s, got %s", key, m.Kind, KindOf(value))
		}
	}
	return nil
}

func (s *Service) SetMetrics(ctx context.Context, actor string, id TicketID, metrics map[string]any) error {
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	panel, _ := s.panels.Get(t.PanelID)
	for k, v := range metrics {
		if err := checkMetric(panel, k, v); err != nil {
			return err
		}
	}
	for k, v := range metrics {
		if err := s.store.UpsertMetric(ctx, id, k, v); err != nil {
			return err
		}
	}
	if s.hooks.AfterSetMetrics != nil {
		return hookErr("AfterSetMetrics", s.hooks.AfterSetMetrics(ctx, actor, id, metrics))
	}
	return nil
}

func (s *Service) RequestClose(ctx context.Context, actor string, id TicketID) (confirmed bool, err error) {
	if err := s.allow(ctx, actor, ActionRequestClose, id); err != nil {
		return false, err
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return false, err
	}

	if t.Status == StatusPendingClose && t.CloseRequestedBy != "" && t.CloseRequestedBy != actor {
		if err := s.allow(ctx, actor, ActionConfirmClose, id); err != nil {
			return false, err
		}
		return s.confirmClose(ctx, actor, id)
	}

	if _, err := s.mutate(ctx, id, func(t *Ticket) error {
		if t.Status != StatusOpen {
			return ErrBadStatus
		}
		t.CloseRequestedBy = actor
		return t.moveTo(StatusPendingClose)
	}); err != nil {
		return false, err
	}
	if s.hooks.AfterRequestClose != nil {
		return false, hookErr("AfterRequestClose", s.hooks.AfterRequestClose(ctx, actor, id))
	}
	return false, nil
}

func (s *Service) ConfirmClose(ctx context.Context, actor string, id TicketID) error {
	if err := s.allow(ctx, actor, ActionConfirmClose, id); err != nil {
		return err
	}
	_, err := s.confirmClose(ctx, actor, id)
	return err
}

func (s *Service) confirmClose(ctx context.Context, actor string, id TicketID) (closed bool, err error) {
	now := time.Now().UTC()
	t, err := s.mutate(ctx, id, func(t *Ticket) error {
		if t.Status != StatusPendingClose {
			return ErrBadStatus
		}
		t.CloseRequestedBy = ""
		return t.moveTo(StatusClosed)
	})
	if err != nil {
		return false, err
	}
	if err := s.finalizeMetrics(ctx, t, now); err != nil {
		return true, err
	}
	if s.hooks.AfterConfirmClose != nil {
		return true, hookErr("AfterConfirmClose", s.hooks.AfterConfirmClose(ctx, actor, id))
	}
	return true, nil
}

func (s *Service) Get(ctx context.Context, id TicketID) (*Ticket, error) {
	return s.store.Get(ctx, id)
}

func (s *Service) Archive(ctx context.Context, id TicketID) error {
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if !t.Status.CanTransitionTo(StatusArchived) {
		return ErrBadStatus
	}
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
