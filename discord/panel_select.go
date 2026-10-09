package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	dgo "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
	"git.aaronf86.tech/aaronf86/ticketbot/messages"
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
		return e.CreateMessage(MsgEphemeral(h.bot.Msgs().System.PanelNoneSelected))
	}

	panelID := sel.Values[0]

	if fn, ok := h.bot.panelModals[panelID]; ok {
		return e.Modal(fn(e.Message.ID))
	}

	if e.GuildID() == nil {
		return e.CreateMessage(MsgEphemeral(h.bot.Msgs().System.PanelGuildOnly))
	}

	actorID := e.User().ID.String()
	result, err := h.bot.CreateTicket(ctx, *e.GuildID(), panelID, actorID)
	if err != nil {
		if errors.Is(err, core.ErrTicketLimitReached) {
			return e.CreateMessage(MsgEphemeral(h.bot.Msgs().Errors.TicketLimit))
		}
		slog.Error("panel select: create ticket", "panel", panelID, "err", err)
		return e.CreateMessage(MsgEphemeral(h.bot.Msgs().System.PanelCreateFailed))
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
			Content: messages.Format(h.bot.Msgs().System.PanelTicketCreated, "channel", result.ChannelID.String()),
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
