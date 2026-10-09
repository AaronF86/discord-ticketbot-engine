package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

const (
	ButtonConfirmClose = "cr-confirm"
	ButtonDeny         = "cr-deny"
	ButtonCloseTicket  = "ticket:close_req"
	ButtonKeepOpen     = "tc:keep_open"
)

type ButtonHandler interface {
	CustomID() string
	Handle(ctx context.Context, e *events.ComponentInteractionCreate) error
}

type PrefixButtonHandler interface {
	Prefix() string
	Handle(ctx context.Context, e *events.ComponentInteractionCreate) error
}

type ButtonRouter struct {
	exact   map[string]ButtonHandler
	aliases map[string]string
	prefix  []PrefixButtonHandler
	onEvent func(ctx context.Context, actor, customID string, err error)

	getError func() string
}

func (r *ButtonRouter) errorMsg() string {
	if r.getError != nil {
		return r.getError()
	}
	return defaultMsgs.System.ModalError
}

func NewButtonRouter(exact []ButtonHandler, prefix []PrefixButtonHandler) *ButtonRouter {
	m := make(map[string]ButtonHandler, len(exact))
	for _, h := range exact {
		m[h.CustomID()] = h
	}
	return &ButtonRouter{exact: m, prefix: prefix}
}

func (r *ButtonRouter) Register(h ButtonHandler)             { r.exact[h.CustomID()] = h }
func (r *ButtonRouter) RegisterPrefix(h PrefixButtonHandler) { r.prefix = append(r.prefix, h) }

func (r *ButtonRouter) Alias(legacy, current string) {
	if r.aliases == nil {
		r.aliases = make(map[string]string)
	}
	r.aliases[legacy] = current
}

func (r *ButtonRouter) SetAuditHook(fn func(ctx context.Context, actor, customID string, err error)) {
	r.onEvent = fn
}

func (r *ButtonRouter) dispatch(ctx context.Context, e *events.ComponentInteractionCreate) {
	cid := e.Data.CustomID()
	if current, ok := r.aliases[cid]; ok {
		cid = current
	}
	actor := e.User().ID.String()

	if h, ok := r.exact[cid]; ok {
		err := h.Handle(ctx, e)
		if err != nil {
			slog.Error("button handler error", "custom_id", cid, "err", err)
			replyComponentErr(e, r.errorMsg())
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
				slog.Error("prefix button handler error", "custom_id", cid, "prefix", h.Prefix(), "err", err)
				replyComponentErr(e, r.errorMsg())
			}
			if r.onEvent != nil {
				r.onEvent(ctx, actor, cid, err)
			}
			return
		}
	}

	slog.Warn("unhandled button interaction", "custom_id", cid)
}

func ButtonID(action, payload string) string {
	return fmt.Sprintf("%s:%s", action, payload)
}

func ButtonPayload(customID string) string {
	_, after, _ := strings.Cut(customID, ":")
	return after
}

func (b *Bot) BuildButton(ctx context.Context, actionType, customID string) discord.ButtonComponent {
	if b.buildButton != nil {
		return b.buildButton(ctx, actionType, customID)
	}
	return defaultButton(ctx, b.cache, b.buttonRepo, actionType, customID)
}

func defaultButton(ctx context.Context, cache *Cache, repo ButtonRepository, actionType, customID string) discord.ButtonComponent {
	if cache != nil {
		if cfg, ok := cache.Button(actionType); ok {
			return styledButton(cfg.Label, customID, cfg.Style, cfg.Emoji)
		}
	}
	if repo != nil {
		cfg, err := repo.GetButton(ctx, actionType)
		if err == nil {
			if cache != nil {
				cache.PutButton(actionType, cfg)
			}
			return styledButton(cfg.Label, customID, cfg.Style, cfg.Emoji)
		}
	}
	slog.Warn("BuildButton: button config not found", "action_type", actionType)
	return discord.NewSecondaryButton(actionType, customID)
}

type ButtonRepository interface {
	GetButton(ctx context.Context, actionType string) (ButtonConfig, error)
}

func styledButton(label, customID string, style int32, emojiID string) discord.ButtonComponent {
	var btn discord.ButtonComponent
	switch style {
	case 1:
		btn = discord.NewPrimaryButton(label, customID)
	case 2:
		btn = discord.NewSecondaryButton(label, customID)
	case 3:
		btn = discord.NewSuccessButton(label, customID)
	case 4:
		btn = discord.NewDangerButton(label, customID)
	default:
		btn = discord.NewSecondaryButton(label, customID)
	}
	if emojiID != "" {
		if id, err := snowflake.Parse(emojiID); err == nil {
			btn = btn.WithEmoji(discord.NewCustomComponentEmoji(id))
		}
	}
	return btn
}
