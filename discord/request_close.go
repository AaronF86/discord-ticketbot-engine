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
	"github.com/disgoorg/disgo/rest"
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

type interactionResponder interface {
	CreateMessage(discord.MessageCreate, ...rest.RequestOpt) error
	ApplicationID() snowflake.ID
	Token() string
}

func (h *CloseRequestHandler) pendingState(id core.TicketID) *crState {
	if raw, ok := h.pending.Load(id); ok {
		return raw.(*crState)
	}
	return nil
}

func (h *CloseRequestHandler) handle(ctx context.Context, e interactionResponder, actorID string, ticket *core.Ticket, channelID snowflake.ID, guildID string) error {
	prev := h.pendingState(ticket.ID)

	msg, accepted, replace, err := h.execute(ctx, actorID, ticket, channelID, guildID)
	if err != nil {
		return e.CreateMessage(MsgEphemeral(h.internalErr()))
	}

	api := h.bot.client.Rest
	hasPrevMsg := prev != nil && prev.msgID != 0
	switch {
	case accepted && hasPrevMsg:
		_, _ = api.UpdateMessage(prev.channelID, prev.msgID,
			discord.NewMessageUpdateV2(discord.NewTextDisplay(h.acceptedMsg())))
	case replace && hasPrevMsg:
		_ = api.DeleteMessage(prev.channelID, prev.msgID)
	}

	if err := e.CreateMessage(msg); err != nil {
		return err
	}
	if replace && !accepted {
		h.trackPrompt(e, ticket.ID)
	}
	return nil
}

func (h *CloseRequestHandler) trackPrompt(e interactionResponder, id core.TicketID) {
	st := h.pendingState(id)
	if st == nil {
		return
	}
	if resp, err := h.bot.client.Rest.GetInteractionResponse(e.ApplicationID(), e.Token()); err == nil && resp != nil {
		st.msgID = resp.ID
	}
}

func (h *CloseRequestHandler) PanelCloseFunc() func(*events.ApplicationCommandInteractionCreate, *core.Ticket) {
	return func(e *events.ApplicationCommandInteractionCreate, ticket *core.Ticket) {
		_ = h.handle(context.Background(), e, e.User().ID.String(), ticket, e.Channel().ID(), guildIDString(e.GuildID()))
	}
}

func (h *CloseRequestHandler) HandleComponentTrigger(ctx context.Context, e *events.ComponentInteractionCreate, ticket *core.Ticket) error {
	actorID := e.User().ID.String()

	if h.reviewHook != nil && slices.Contains(ticket.Users, actorID) {
		if h.pendingState(ticket.ID) == nil {
			return h.reviewHook(e, ticket.ID, e.Channel().ID())
		}
	}
	return h.handle(ctx, e, actorID, ticket, e.Channel().ID(), guildIDString(e.GuildID()))
}

func (h *CloseRequestHandler) HandleModalClose(ctx context.Context, actorID string, channelID snowflake.ID, guildID string, e *events.ModalSubmitInteractionCreate, ticket *core.Ticket) error {
	return h.handle(ctx, e, actorID, ticket, channelID, guildID)
}

func guildIDString(id *snowflake.ID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func (h *CloseRequestHandler) ButtonHandlers() (confirm, deny PrefixButtonHandler) {
	return &crConfirmHandler{h}, &crDenyHandler{h}
}

func (h *CloseRequestHandler) execute(ctx context.Context, actorID string, ticket *core.Ticket, channelID snowflake.ID, guildID string) (msg discord.MessageCreate, accepted, replace bool, err error) {
	isOwner := actorID == ticket.CreatedBy
	isStaff := false
	if !isOwner {
		isStaff, _ = h.bot.auth.Allow(ctx, actorID, core.ActionClose, ticket.ID)
		if !isStaff {
			return MsgEphemeral(h.forbiddenMsg()), false, false, nil
		}
	}

	if st := h.pendingState(ticket.ID); st != nil {
		if (st.requestorIsOwner && isStaff) || (!st.requestorIsOwner && isOwner) {
			return h.acceptPending(ctx, actorID, ticket, channelID, st)
		}
		return h.buildMessage(ctx, actorID, ticket.CreatedBy, ticket.ID, isOwner, guildID), false, true, nil
	}
	return h.openRequest(ctx, actorID, ticket, channelID, guildID, isOwner)
}

func (h *CloseRequestHandler) acceptPending(ctx context.Context, actorID string, ticket *core.Ticket, channelID snowflake.ID, st *crState) (msg discord.MessageCreate, accepted, replace bool, err error) {
	h.pending.Delete(ticket.ID)
	if confirmErr := h.bot.svc.ConfirmClose(ctx, actorID, ticket.ID); confirmErr != nil {
		var hookErr *core.HookError
		if !errors.As(confirmErr, &hookErr) {
			return MsgEphemeral(h.internalErr()), false, false, confirmErr
		}
		slog.Error("confirm close hook failed", "ticket", ticket.ID, "err", confirmErr)
	}
	h.bot.archiveAndClose(channelID, ticket, st.panelName)
	return discord.MessageCreate{Content: h.bot.closeSuccessMsg(ticket.PanelID)}, true, false, nil
}

func (h *CloseRequestHandler) openRequest(ctx context.Context, actorID string, ticket *core.Ticket, channelID snowflake.ID, guildID string, isOwner bool) (msg discord.MessageCreate, accepted, replace bool, err error) {
	panelName := ticket.PanelID
	if panel, _ := h.bot.svc.Panel(ticket.PanelID); panel != nil {
		panelName = panel.Name
	}

	confirmed, err := h.bot.svc.RequestClose(ctx, actorID, ticket.ID)
	if confirmed {
		if err != nil {
			slog.Error("close request auto-confirm: post-close step failed", "ticket", ticket.ID, "err", err)
		}
		h.bot.archiveAndClose(channelID, ticket, panelName)
		return discord.MessageCreate{Content: h.bot.closeSuccessMsg(ticket.PanelID)}, true, false, nil
	}
	if err != nil {
		return h.requestFailed(ctx, err, ticket, channelID, guildID, panelName)
	}

	h.pending.Store(ticket.ID, &crState{
		requestorID:      actorID,
		requestorIsOwner: isOwner,
		channelID:        channelID,
		panelName:        panelName,
	})
	return h.buildMessage(ctx, actorID, ticket.CreatedBy, ticket.ID, isOwner, guildID), false, false, nil
}

func (h *CloseRequestHandler) requestFailed(ctx context.Context, reqErr error, ticket *core.Ticket, channelID snowflake.ID, guildID, panelName string) (msg discord.MessageCreate, accepted, replace bool, err error) {
	if errors.Is(reqErr, core.ErrBadStatus) {
		fresh, fetchErr := h.bot.svc.Get(ctx, ticket.ID)
		if fetchErr == nil && fresh.Status == core.StatusPendingClose && fresh.CloseRequestedBy != "" {
			requestorIsOwner := fresh.CloseRequestedBy == fresh.CreatedBy
			h.pending.Store(ticket.ID, &crState{
				requestorID:      fresh.CloseRequestedBy,
				requestorIsOwner: requestorIsOwner,
				channelID:        channelID,
				panelName:        panelName,
			})
			return h.buildMessage(ctx, fresh.CloseRequestedBy, fresh.CreatedBy, ticket.ID, requestorIsOwner, guildID), false, false, nil
		}
	}
	return MsgEphemeral(h.internalErr()), false, false, nil
}

func (h *CloseRequestHandler) buildMessage(ctx context.Context, actorID, ownerID string, ticketID core.TicketID, isOwner bool, guildID string) discord.MessageCreate {
	if h.bot.templateService == nil {
		return MsgEphemeral(h.bot.Msgs().CloseRequest.Sent)
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
		return MsgEphemeral(h.bot.Msgs().CloseRequest.Sent)
	}

	id := strconv.FormatInt(int64(ticketID), 10)
	components := make([]discord.LayoutComponent, 0, len(tmplComponents)+1)
	components = append(components, tmplComponents...)
	components = append(components, discord.NewActionRow(
		h.bot.BuildButton(ctx, ButtonConfirmClose, prefixCRConfirm+id),
		h.bot.BuildButton(ctx, ButtonDeny, prefixCRDeny+id),
	))
	return discord.NewMessageCreateV2(components...)
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
		return e.CreateMessage(MsgEphemeral(msgs.Errors.InvalidID))
	}
	ticketID := core.TicketID(n)

	st := c.h.pendingState(ticketID)
	if st == nil {
		return e.CreateMessage(MsgEphemeral(msgs.CloseRequest.NoPending))
	}
	actorID := e.User().ID.String()

	if actorID == st.requestorID {
		return e.CreateMessage(MsgEphemeral(msgs.CloseRequest.CannotConfirmOwn))
	}

	ticket, err := c.h.bot.svc.Get(ctx, ticketID)
	if err != nil {
		return fmt.Errorf("get ticket: %w", err)
	}
	confirmerIsOwner := actorID == ticket.CreatedBy
	confirmerIsStaff, _ := c.h.bot.auth.Allow(ctx, actorID, core.ActionClose, ticketID)

	if reason := confirmRejection(msgs, st, confirmerIsOwner, confirmerIsStaff); reason != "" {
		return e.CreateMessage(MsgEphemeral(reason))
	}

	c.h.pending.Delete(ticketID)

	if err := c.h.bot.svc.ConfirmClose(ctx, actorID, ticketID); err != nil {
		return fmt.Errorf("confirm close: %w", err)
	}

	_ = e.UpdateMessage(discord.NewMessageUpdateV2(discord.NewTextDisplay(c.h.acceptedMsg())))
	c.h.bot.archiveAndClose(st.channelID, ticket, st.panelName)
	return nil
}

func confirmRejection(msgs *messages.Messages, st *crState, isOwner, isStaff bool) string {
	switch {
	case st.requestorIsOwner && (isOwner || !isStaff):
		return msgs.CloseRequest.StaffMustConfirm
	case !st.requestorIsOwner && !isOwner:
		return msgs.CloseRequest.OwnerMustConfirm
	}
	return ""
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
