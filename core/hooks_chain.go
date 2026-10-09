package core

import "context"

type (
	ticketHook  = func(context.Context, *Ticket) error
	actorHook   = func(context.Context, string, TicketID) error
	userHook    = func(context.Context, string, TicketID, string) error
	statusHook  = func(context.Context, string, TicketID, Status) error
	metricsHook = func(context.Context, string, TicketID, map[string]any) error
	metaHook    = func(context.Context, string, TicketID, map[string]string) error
)

func ChainHooks(hs ...Hooks) Hooks {
	return Hooks{
		AfterOpen:         join1(pick(hs, func(h Hooks) ticketHook { return h.AfterOpen })),
		BeforeClose:       join2(pick(hs, func(h Hooks) actorHook { return h.BeforeClose })),
		AfterClose:        join2(pick(hs, func(h Hooks) actorHook { return h.AfterClose })),
		BeforeForceClose:  join2(pick(hs, func(h Hooks) actorHook { return h.BeforeForceClose })),
		AfterForceClose:   join2(pick(hs, func(h Hooks) actorHook { return h.AfterForceClose })),
		AfterAddUser:      joinArg(pick(hs, func(h Hooks) userHook { return h.AfterAddUser })),
		AfterRemoveUser:   joinArg(pick(hs, func(h Hooks) userHook { return h.AfterRemoveUser })),
		AfterClaim:        join2(pick(hs, func(h Hooks) actorHook { return h.AfterClaim })),
		AfterUnclaim:      join2(pick(hs, func(h Hooks) actorHook { return h.AfterUnclaim })),
		AfterSetStatus:    joinArg(pick(hs, func(h Hooks) statusHook { return h.AfterSetStatus })),
		AfterRequestClose: join2(pick(hs, func(h Hooks) actorHook { return h.AfterRequestClose })),
		AfterConfirmClose: join2(pick(hs, func(h Hooks) actorHook { return h.AfterConfirmClose })),
		AfterSetMetrics:   joinArg(pick(hs, func(h Hooks) metricsHook { return h.AfterSetMetrics })),
		AfterSetMeta:      joinArg(pick(hs, func(h Hooks) metaHook { return h.AfterSetMeta })),
	}
}

func pick[F any](hs []Hooks, get func(Hooks) F) []F {
	out := make([]F, len(hs))
	for i, h := range hs {
		out[i] = get(h)
	}
	return out
}

func join1(fns []ticketHook) ticketHook {
	live := fns[:0:0]
	for _, f := range fns {
		if f != nil {
			live = append(live, f)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return func(ctx context.Context, t *Ticket) error {
		for _, f := range live {
			if err := f(ctx, t); err != nil {
				return err
			}
		}
		return nil
	}
}

func join2(fns []actorHook) actorHook {
	live := fns[:0:0]
	for _, f := range fns {
		if f != nil {
			live = append(live, f)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return func(ctx context.Context, actor string, id TicketID) error {
		for _, f := range live {
			if err := f(ctx, actor, id); err != nil {
				return err
			}
		}
		return nil
	}
}

func joinArg[X any](fns []func(context.Context, string, TicketID, X) error) func(context.Context, string, TicketID, X) error {
	live := fns[:0:0]
	for _, f := range fns {
		if f != nil {
			live = append(live, f)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return func(ctx context.Context, actor string, id TicketID, x X) error {
		for _, f := range live {
			if err := f(ctx, actor, id, x); err != nil {
				return err
			}
		}
		return nil
	}
}
