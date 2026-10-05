package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	dgo "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
)

const panelSelectPrefix = "panel:"

type PanelSelectHandler struct {
	bot *Bot
}

func NewPanelSelectHandler(bot *Bot) *PanelSelectHandler {
	return &PanelSelectHandler{bot: bot}
}

func (h *PanelSelectHandler) Prefix() string { return panelSelectPrefix }

func (h *PanelSelectHandler) Handle(ctx context.Context, e *events.ComponentInteractionCreate) error {
	sel, ok := e.Data.(dgo.StringSelectMenuInteractionData)
	if !ok || len(sel.Values) == 0 {
		return e.CreateMessage(MsgEphemeral("No panel selected."))
	}

	panelID := sel.Values[0]

	if fn, ok := h.bot.panelModals[panelID]; ok {
		return e.Modal(fn(e.Message.ID))
	}

	if e.GuildID() == nil {
		msgs := h.bot.Msgs()
		msg := "This can only be used in a server."
		if msgs != nil && msgs.System.PanelGuildOnly != "" {
			msg = msgs.System.PanelGuildOnly
		}
		return e.CreateMessage(MsgEphemeral(msg))
	}

	actorID := e.User().ID.String()
	result, err := h.bot.CreateTicket(ctx, *e.GuildID(), panelID, actorID)
	if err != nil {
		if errors.Is(err, core.ErrTicketLimitReached) {
			msgs := h.bot.Msgs()
			msg := "You already have an open ticket for this panel."
			if msgs != nil {
				msg = msgs.Errors.TicketLimit
			}
			return e.CreateMessage(MsgEphemeral(msg))
		}
		slog.Error("panel select: create ticket", "panel", panelID, "err", err)
		return e.CreateMessage(MsgEphemeral(fmt.Sprintf("Failed to open ticket: %v", err)))
	}

	freshComponents, err := h.resetPanelPrompt(ctx)
	if err == nil {
		_ = e.UpdateMessage(dgo.NewMessageUpdateV2(freshComponents...))
	} else {
		slog.Warn("panel select: could not reset prompt", "err", err)
		_ = e.UpdateMessage(dgo.NewMessageUpdateV2())
	}

	_, _ = e.Client().Rest.CreateFollowupMessage(
		e.ApplicationID(), e.Token(),
		dgo.MessageCreate{
			Content: fmt.Sprintf("Ticket created: <#%s>", result.ChannelID),
			Flags:   dgo.MessageFlagEphemeral,
		},
	)
	return nil
}

func (h *PanelSelectHandler) resetPanelPrompt(ctx context.Context) ([]dgo.LayoutComponent, error) {
	if h.bot.templateService == nil {
		return nil, fmt.Errorf("no template service configured")
	}
	components, _, err := h.bot.templateService.CompileWithPanels(
		ctx, "ticket-creation-prompt", nil, h.bot.svc.AllPanels(),
	)
	return components, err
}
