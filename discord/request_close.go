package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
	"git.aaronf86.tech/aaronf86/ticketbot/messages"
)

const (
	TemplateCloseRequestOwner = "close_request_owner"
	TemplateCloseRequestStaff = "close_request_staff"
)

const (
	prefixCRConfirm = "cr-confirm:"
	prefixCRDeny    = "cr-deny:"
)

type crState struct {
	requestorID      string
	requestorIsOwner bool
	channelID        snowflake.ID
	panelName        string
	msgID            snowflake.ID
}

type reviewHookFn func(e *events.ComponentInteractionCreate, ticketID core.TicketID, channelID snowflake.ID) error

type CloseRequestHandler struct {
	bot           *Bot
	pending       sync.Map
	reviewHook    reviewHookFn
	SupportRoleFn func(ctx context.Context, guildID string) string
}

func NewCloseRequestHandler(bot *Bot) *CloseRequestHandler {
	return &CloseRequestHandler{bot: bot}
}

func (h *CloseRequestHandler) SetReviewHook(fn reviewHookFn) { h.reviewHook = fn }

func (h *CloseRequestHandler) PanelCloseFunc() func(*events.ApplicationCommandInteractionCreate, *core.Ticket) {
	return func(e *events.ApplicationCommandInteractionCreate, ticket *core.Ticket) {
		ctx := context.Background()
		guildID := ""
		if e.GuildID() != nil {
			guildID = e.GuildID().String()
		}

		var prev *crState
		if raw, ok := h.pending.Load(ticket.ID); ok {
			prev = raw.(*crState)
		}

		msg, accepted, replace, err := h.execute(ctx, e.User().ID.String(), ticket, e.Channel().ID(), guildID)
		if err != nil {
			_ = e.CreateMessage(MsgEphemeral(h.internalErr()))
			return
		}
		if accepted {
			if prev != nil && prev.msgID != 0 {
				_, _ = h.bot.client.Rest.UpdateMessage(prev.channelID, prev.msgID,
					discord.NewMessageUpdateV2(discord.NewTextDisplay(h.acceptedMsg())))
			}
			_ = e.CreateMessage(msg)
			return
		}
		if replace && prev != nil && prev.msgID != 0 {
			_ = h.bot.client.Rest.DeleteMessage(prev.channelID, prev.msgID)
		}
		_ = e.CreateMessage(msg)
		if replace {
			if raw, ok := h.pending.Load(ticket.ID); ok {
				st := raw.(*crState)
				if resp, fetchErr := h.bot.client.Rest.GetInteractionResponse(e.ApplicationID(), e.Token()); fetchErr == nil && resp != nil {
					st.msgID = resp.ID
				}
			}
		}
	}
}

func (h *CloseRequestHandler) HandleComponentTrigger(ctx context.Context, e *events.ComponentInteractionCreate, ticket *core.Ticket) error {
	actorID := e.User().ID.String()

	if h.reviewHook != nil && slices.Contains(ticket.Users, actorID) {
		if _, hasPending := h.pending.Load(ticket.ID); !hasPending {
			return h.reviewHook(e, ticket.ID, e.Channel().ID())
		}
	}

	guildID := ""
	if e.GuildID() != nil {
		guildID = e.GuildID().String()
	}

	var prev *crState
	if raw, ok := h.pending.Load(ticket.ID); ok {
		prev = raw.(*crState)
	}

	msg, accepted, replace, err := h.execute(ctx, actorID, ticket, e.Channel().ID(), guildID)
	if err != nil {
		return e.CreateMessage(MsgEphemeral(h.internalErr()))
	}
	if accepted {
		if prev != nil && prev.msgID != 0 {
			_, _ = h.bot.client.Rest.UpdateMessage(prev.channelID, prev.msgID,
				discord.NewMessageUpdateV2(discord.NewTextDisplay(h.acceptedMsg())))
		}
		return e.CreateMessage(msg)
	}
	if replace && prev != nil && prev.msgID != 0 {
		_ = h.bot.client.Rest.DeleteMessage(prev.channelID, prev.msgID)
	}
	if err := e.CreateMessage(msg); err != nil {
		return err
	}
	if replace {
		if raw, ok := h.pending.Load(ticket.ID); ok {
			st := raw.(*crState)
			if resp, fetchErr := h.bot.client.Rest.GetInteractionResponse(e.ApplicationID(), e.Token()); fetchErr == nil && resp != nil {
				st.msgID = resp.ID
			}
		}
	}
	return nil
}

func (h *CloseRequestHandler) HandleModalClose(ctx context.Context, actorID string, channelID snowflake.ID, guildID string, e *events.ModalSubmitInteractionCreate, ticket *core.Ticket) error {
	var prev *crState
	if raw, ok := h.pending.Load(ticket.ID); ok {
		prev = raw.(*crState)
	}

	msg, accepted, replace, err := h.execute(ctx, actorID, ticket, channelID, guildID)
	if err != nil {
		return e.CreateMessage(MsgEphemeral(h.internalErr()))
	}
	if accepted {
		if prev != nil && prev.msgID != 0 {
			_, _ = h.bot.client.Rest.UpdateMessage(prev.channelID, prev.msgID,
				discord.NewMessageUpdateV2(discord.NewTextDisplay(h.acceptedMsg())))
		}
		return e.CreateMessage(msg)
	}
	if replace && prev != nil && prev.msgID != 0 {
		_ = h.bot.client.Rest.DeleteMessage(prev.channelID, prev.msgID)
	}
	if err := e.CreateMessage(msg); err != nil {
		return err
	}
	if replace {
		if raw, ok := h.pending.Load(ticket.ID); ok {
			st := raw.(*crState)
			if resp, fetchErr := h.bot.client.Rest.GetInteractionResponse(e.ApplicationID(), e.Token()); fetchErr == nil && resp != nil {
				st.msgID = resp.ID
			}
		}
	}
	return nil
}

func (h *CloseRequestHandler) ButtonHandlers() (confirm, deny PrefixButtonHandler) {
	return &crConfirmHandler{h}, &crDenyHandler{h}
}

func (h *CloseRequestHandler) execute(ctx context.Context, actorID string, ticket *core.Ticket, channelID snowflake.ID, guildID string) (msg discord.MessageCreate, accepted bool, replace bool, err error) {
	isOwner := actorID == ticket.CreatedBy
	isStaff := false
	if !isOwner {
		isStaff, _ = h.bot.auth.Allow(ctx, actorID, core.ActionClose, ticket.ID)
		if !isStaff {
			return MsgEphemeral(h.forbiddenMsg()), false, false, nil
		}
	}

	if raw, exists := h.pending.Load(ticket.ID); exists {
		st := raw.(*crState)
		canAccept := (st.requestorIsOwner && isStaff) || (!st.requestorIsOwner && isOwner)
		if canAccept {
			h.pending.Delete(ticket.ID)
			if confirmErr := h.bot.svc.ConfirmClose(ctx, actorID, ticket.ID); confirmErr != nil {
				return MsgEphemeral(h.internalErr()), false, false, confirmErr
			}
			h.bot.archiveAndClose(channelID, ticket, st.panelName)
			return discord.MessageCreate{Content: h.bot.closeSuccessMsg(ticket.PanelID)}, true, false, nil
		}
		built, buildErr := h.buildMessage(ctx, actorID, ticket.CreatedBy, ticket.ID, isOwner, guildID)
		return built, false, true, buildErr
	}

	panel, _ := h.bot.svc.Panel(ticket.PanelID)
	panelName := ticket.PanelID
	if panel != nil {
		panelName = panel.Name
	}

	if err := h.bot.svc.RequestClose(ctx, actorID, ticket.ID); err != nil {
		if errors.Is(err, core.ErrAutoConfirmed) {
			h.bot.archiveAndClose(channelID, ticket, panelName)
			return discord.MessageCreate{Content: h.bot.closeSuccessMsg(ticket.PanelID)}, true, false, nil
		}
		if errors.Is(err, core.ErrBadStatus) {
			fresh, fetchErr := h.bot.svc.Get(ctx, ticket.ID)
			if fetchErr == nil && fresh.Status == core.StatusPendingClose && fresh.CloseRequestedBy != "" {
				requestorIsOwner := fresh.CloseRequestedBy == fresh.CreatedBy
				h.pending.Store(ticket.ID, &crState{
					requestorID:      fresh.CloseRequestedBy,
					requestorIsOwner: requestorIsOwner,
					channelID:        channelID,
					panelName:        panelName,
				})
				built, buildErr := h.buildMessage(ctx, fresh.CloseRequestedBy, fresh.CreatedBy, ticket.ID, requestorIsOwner, guildID)
				return built, false, false, buildErr
			}
		}
		return MsgEphemeral(h.internalErr()), false, false, nil
	}

	h.pending.Store(ticket.ID, &crState{
		requestorID:      actorID,
		requestorIsOwner: isOwner,
		channelID:        channelID,
		panelName:        panelName,
	})
	built, buildErr := h.buildMessage(ctx, actorID, ticket.CreatedBy, ticket.ID, isOwner, guildID)
	return built, false, false, buildErr
}

func (h *CloseRequestHandler) buildMessage(ctx context.Context, actorID, ownerID string, ticketID core.TicketID, isOwner bool, guildID string) (discord.MessageCreate, error) {
	if h.bot.templateService == nil {
		return MsgEphemeral("Close request sent."), nil
	}

	var templateID string
	var party string
	if isOwner {
		templateID = TemplateCloseRequestOwner
		if guildID != "" && h.SupportRoleFn != nil {
			if roleID := h.SupportRoleFn(ctx, guildID); roleID != "" {
				party = fmt.Sprintf("<@&%s>", roleID)
			}
		}
	} else {
		templateID = TemplateCloseRequestStaff
		party = fmt.Sprintf("<@%s>", ownerID)
	}

	tmplComponents, err := h.bot.templateService.Compile(ctx, templateID, map[string]string{
		"Closer":         party,
		"CLOSEREQUESTER": fmt.Sprintf("<@%s>", actorID),
	})
	if err != nil {
		slog.Warn("close request: template compile failed, falling back", "err", err)
		return MsgEphemeral("Close request sent."), nil
	}

	id := strconv.FormatInt(int64(ticketID), 10)
	components := make([]discord.LayoutComponent, 0, len(tmplComponents)+1)
	components = append(components, tmplComponents...)
	components = append(components, discord.NewActionRow(
		h.bot.BuildButton(ctx, ButtonConfirmClose, prefixCRConfirm+id),
		h.bot.BuildButton(ctx, ButtonDeny, prefixCRDeny+id),
	))
	return discord.NewMessageCreateV2(components...), nil
}

func (h *CloseRequestHandler) internalErr() string {
	if msgs := h.bot.Msgs(); msgs != nil {
		return msgs.Errors.Internal
	}
	return "An internal error occurred."
}

func (h *CloseRequestHandler) forbiddenMsg() string {
	if msgs := h.bot.Msgs(); msgs != nil {
		return msgs.Errors.Forbidden
	}
	return "Permission denied."
}

func (h *CloseRequestHandler) acceptedMsg() string {
	if msgs := h.bot.Msgs(); msgs != nil {
		return msgs.CloseRequest.Accepted
	}
	return "Close request accepted."
}

type crConfirmHandler struct{ h *CloseRequestHandler }

func (c *crConfirmHandler) Prefix() string { return prefixCRConfirm }

func (c *crConfirmHandler) Handle(ctx context.Context, e *events.ComponentInteractionCreate) error {
	msgs := c.h.bot.Msgs()
	n, err := strconv.ParseInt(strings.TrimPrefix(e.Data.CustomID(), prefixCRConfirm), 10, 64)
	if err != nil {
		msg := "Invalid ID."
		if msgs != nil {
			msg = msgs.Errors.InvalidID
		}
		return e.CreateMessage(MsgEphemeral(msg))
	}
	ticketID := core.TicketID(n)

	raw, ok := c.h.pending.Load(ticketID)
	if !ok {
		msg := "No pending close request."
		if msgs != nil {
			msg = msgs.CloseRequest.NoPending
		}
		return e.CreateMessage(MsgEphemeral(msg))
	}
	st := raw.(*crState)
	actorID := e.User().ID.String()

	if actorID == st.requestorID {
		msg := "You cannot confirm your own close request."
		if msgs != nil {
			msg = msgs.CloseRequest.CannotConfirmOwn
		}
		return e.CreateMessage(MsgEphemeral(msg))
	}

	ticket, err := c.h.bot.svc.Get(ctx, ticketID)
	if err != nil {
		return fmt.Errorf("get ticket: %w", err)
	}
	confirmerIsOwner := actorID == ticket.CreatedBy
	confirmerIsStaff, _ := c.h.bot.auth.Allow(ctx, actorID, core.ActionClose, ticketID)

	if st.requestorIsOwner {
		if confirmerIsOwner || !confirmerIsStaff {
			msg := "A staff member must confirm this close request."
			if msgs != nil {
				msg = msgs.CloseRequest.StaffMustConfirm
			}
			return e.CreateMessage(MsgEphemeral(msg))
		}
	} else {
		if !confirmerIsOwner {
			msg := "The ticket owner must confirm this close request."
			if msgs != nil {
				msg = msgs.CloseRequest.OwnerMustConfirm
			}
			return e.CreateMessage(MsgEphemeral(msg))
		}
	}

	c.h.pending.Delete(ticketID)

	if err := c.h.bot.svc.ConfirmClose(ctx, actorID, ticketID); err != nil {
		return fmt.Errorf("confirm close: %w", err)
	}

	_ = e.UpdateMessage(discord.NewMessageUpdateV2(discord.NewTextDisplay(c.h.acceptedMsg())))
	c.h.bot.archiveAndClose(st.channelID, ticket, st.panelName)
	return nil
}

type crDenyHandler struct{ h *CloseRequestHandler }

func (d *crDenyHandler) Prefix() string { return prefixCRDeny }

func (d *crDenyHandler) Handle(ctx context.Context, e *events.ComponentInteractionCreate) error {
	msgs := d.h.bot.Msgs()
	n, _ := strconv.ParseInt(strings.TrimPrefix(e.Data.CustomID(), prefixCRDeny), 10, 64)
	ticketID := core.TicketID(n)
	actorID := e.User().ID.String()

	ticket, err := d.h.bot.svc.Get(ctx, ticketID)
	if err != nil {
		msg := "An internal error occurred."
		if msgs != nil {
			msg = msgs.Errors.Internal
		}
		return e.CreateMessage(MsgEphemeral(msg))
	}
	isOwner := actorID == ticket.CreatedBy
	isStaff, _ := d.h.bot.auth.Allow(ctx, actorID, core.ActionClose, ticketID)
	if !isOwner && !isStaff {
		msg := "Permission denied."
		if msgs != nil {
			msg = msgs.Errors.Forbidden
		}
		return e.CreateMessage(MsgEphemeral(msg))
	}

	d.h.pending.Delete(ticketID)
	_ = d.h.bot.svc.CancelPendingClose(ctx, ticketID)
	msg := fmt.Sprintf("<@%s> rejected the close request.", e.User().ID)
	if msgs != nil {
		msg = messages.Format(msgs.Ticket.CloseCancel, "user", fmt.Sprintf("<@%s>", e.User().ID))
	}
	return e.UpdateMessage(discord.NewMessageUpdateV2(discord.NewTextDisplay(msg)))
}
