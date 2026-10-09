package discord

import (
	"context"
	"log/slog"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
)

type ModalHandler interface {
	ModalID() string
	Handle(ctx context.Context, e *events.ModalSubmitInteractionCreate) error
}

type PrefixModalHandler interface {
	Prefix() string
	Handle(ctx context.Context, e *events.ModalSubmitInteractionCreate) error
}

type ModalRouter struct {
	exact         map[string]ModalHandler
	prefix        []PrefixModalHandler
	onEvent       func(ctx context.Context, actor, customID string, err error)
	getModalError func() string
}

func (r *ModalRouter) SetModalError(fn func() string) { r.getModalError = fn }

func NewModalRouter(exact []ModalHandler, prefix []PrefixModalHandler) *ModalRouter {
	m := make(map[string]ModalHandler, len(exact))
	for _, h := range exact {
		m[h.ModalID()] = h
	}
	return &ModalRouter{exact: m, prefix: prefix}
}

func (r *ModalRouter) Register(h ModalHandler)             { r.exact[h.ModalID()] = h }
func (r *ModalRouter) RegisterPrefix(h PrefixModalHandler) { r.prefix = append(r.prefix, h) }

func (r *ModalRouter) SetAuditHook(fn func(ctx context.Context, actor, customID string, err error)) {
	r.onEvent = fn
}

func (r *ModalRouter) dispatch(ctx context.Context, e *events.ModalSubmitInteractionCreate) {
	cid := e.Data.CustomID
	actor := e.User().ID.String()

	if h, ok := r.exact[cid]; ok {
		err := h.Handle(ctx, e)
		if err != nil {
			slog.Error("modal handler error", "custom_id", cid, "err", err)
			r.replyErr(e)
		}
		if r.onEvent != nil {
			r.onEvent(ctx, actor, cid, err)
		}
		return
	}

	for _, h := range r.prefix {
		if strings.HasPrefix(cid, h.Prefix()) {
			err := h.Handle(ctx, e)
			if err != nil {
				slog.Error("prefix modal handler error", "custom_id", cid, "prefix", h.Prefix(), "err", err)
				r.replyErr(e)
			}
			if r.onEvent != nil {
				r.onEvent(ctx, actor, cid, err)
			}
			return
		}
	}

	slog.Warn("unhandled modal submission", "custom_id", cid)
}

func (r *ModalRouter) replyErr(e *events.ModalSubmitInteractionCreate) {
	msg := defaultMsgs.System.ModalError
	if r.getModalError != nil {
		if s := r.getModalError(); s != "" {
			msg = s
		}
	}
	_ = e.CreateMessage(discord.MessageCreate{Content: msg, Flags: discord.MessageFlagEphemeral})
}
