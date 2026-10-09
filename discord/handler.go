package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
	"git.aaronf86.tech/aaronf86/ticketbot/messages"
)

const interactionTimeout = 60 * time.Second

func (b *Bot) handleOpen(e *events.ApplicationCommandInteractionCreate) {
	panelID := e.SlashCommandInteractionData().String("panel")

	if fn, ok := b.panelModals[panelID]; ok {
		_ = e.Modal(fn(0))
		return
	}

	guildID := e.GuildID()
	if guildID == nil {
		b.replyErr(e, b.Msgs().System.PanelGuildOnly)
		return
	}

	if err := e.DeferCreateMessage(true); err != nil {
		slog.Error("defer open interaction", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), interactionTimeout)
	defer cancel()
	reply := func(content string) {
		_, _ = e.Client().Rest.UpdateInteractionResponse(e.ApplicationID(), e.Token(), discord.MessageUpdate{Content: &content})
	}

	actor := e.User().ID
	result, err := b.CreateTicket(ctx, *guildID, panelID, actor.String())
	if err != nil {
		slog.Error("ticket.open failed", "panel", panelID, "user", actor.String(), "err", err)
		reply(b.closeErrMsg(err))
		return
	}

	slog.Info("ticket.open", "ticket_id", result.TicketID, "panel", panelID, "user", actor.String(), "channel", result.ChannelID.String())
	reply(messages.Format(b.Msgs().Ticket.OpenSuccess, "channel", result.ChannelID.String()))
}

func (b *Bot) handleClose(e *events.ApplicationCommandInteractionCreate) {
	ctx, actor, ticketID, ok := b.resolveTicket(e)
	if !ok {
		return
	}
	channelID := e.Channel().ID()

	ticket, panelName := b.ticketAndPanel(ctx, ticketID)
	if h, ok := b.panelCloseHandlers[ticket.PanelID]; ok {
		h(e, ticket)
		return
	}

	if err := b.svc.Close(ctx, actor, ticketID); !applied("close", ticketID, err) {
		b.replyErr(e, b.closeErrMsg(err))
		return
	}

	slog.Info("ticket.close", "ticket_id", ticketID, "user", actor)
	_ = e.CreateMessage(discord.MessageCreate{Content: b.closeSuccessMsg(ticket.PanelID)})
	b.archiveAndClose(channelID, ticket, panelName)
}

func (b *Bot) handleForceClose(e *events.ApplicationCommandInteractionCreate) {
	ctx, actor, ticketID, ok := b.resolveTicket(e)
	if !ok {
		return
	}
	channelID := e.Channel().ID()
	msgs := b.Msgs()

	delayStr, _ := e.SlashCommandInteractionData().OptString("delay")
	if delayStr == "" {
		b.forceCloseNow(ctx, e, actor, ticketID, channelID)
		return
	}

	delay, ok := parseDelay(delayStr)
	if !ok {
		b.replyErr(e, msgs.Ticket.ForceCloseInvalidDelay)
		return
	}

	closeAt := time.Now().Add(delay)
	scheduled, err := b.scheduleForceClose(ctx, ticketID, channelID, actor, closeAt)
	if err != nil {
		slog.Error("schedule force close", "err", err)
		b.replyErr(e, msgs.Errors.Internal)
		return
	}
	if !scheduled {
		b.replyErr(e, msgs.Ticket.ForceCloseAlreadyScheduled)
		return
	}

	ts := fmt.Sprintf("<t:%d:R>", closeAt.Unix())
	_ = e.CreateMessage(discord.MessageCreate{Content: messages.Format(msgs.Ticket.ForceCloseScheduled, "time", ts)})
}

func (b *Bot) forceCloseNow(ctx context.Context, e *events.ApplicationCommandInteractionCreate, actor string, ticketID core.TicketID, channelID snowflake.ID) {
	if err := b.svc.ForceClose(ctx, actor, ticketID); !applied("force close", ticketID, err) {
		b.replyErr(e, b.closeErrMsg(err))
		return
	}
	ticket, panelName := b.ticketAndPanel(ctx, ticketID)
	slog.Info("ticket.force_close", "ticket_id", ticketID, "user", actor)
	_ = e.CreateMessage(discord.MessageCreate{Content: b.forceCloseSuccessMsg(ticket.PanelID)})
	b.archiveAndClose(channelID, ticket, panelName)
}

func (b *Bot) handleTimedClose(e *events.ApplicationCommandInteractionCreate) {
	ctx, actor, ticketID, ok := b.resolveTicket(e)
	if !ok {
		return
	}
	channelID := e.Channel().ID()
	msgs := b.Msgs()

	allowed, _ := b.auth.Allow(ctx, actor, core.ActionForceClose, ticketID)
	if !allowed {
		b.replyErr(e, b.ForbiddenMsg("do that"))
		return
	}

	delay := 24 * time.Hour
	if delayStr, _ := e.SlashCommandInteractionData().OptString("delay"); delayStr != "" {
		d, ok := parseDelay(delayStr)
		if !ok {
			b.replyErr(e, msgs.TimedClose.InvalidDelay)
			return
		}
		delay = d
	}

	ticket, err := b.svc.Get(ctx, ticketID)
	if err != nil {
		b.replyErr(e, msgs.Errors.Internal)
		return
	}

	scheduled, err := b.scheduleTimedClose(ctx, ticketID, channelID, actor, ticket.CreatedBy, time.Now().Add(delay))
	if err != nil {
		slog.Error("schedule timed close", "err", err)
		b.replyErr(e, msgs.Errors.Internal)
		return
	}
	if !scheduled {
		b.replyErr(e, msgs.TimedClose.AlreadyScheduled)
		return
	}

	_ = e.CreateMessage(MsgEphemeral(msgs.TimedClose.ScheduledAck))
}

func (b *Bot) handleClaim(e *events.ApplicationCommandInteractionCreate) {
	ctx, actor, ticketID, ok := b.resolveTicket(e)
	if !ok {
		return
	}

	if err := b.svc.Claim(ctx, actor, ticketID); err != nil {
		b.replyErr(e, b.closeErrMsg(err))
		return
	}

	slog.Info("ticket.claim", "ticket_id", ticketID, "user", actor)
	msgs := b.Msgs()
	msg := fmt.Sprintf("<@%s> claimed this ticket.", e.User().ID)
	if msgs != nil {
		msg = messages.Format(msgs.Ticket.Claimed, "agent", fmt.Sprintf("<@%s>", e.User().ID))
	}
	_ = e.CreateMessage(discord.MessageCreate{Content: msg})
}

func (b *Bot) handleUnclaim(e *events.ApplicationCommandInteractionCreate) {
	ctx, actor, ticketID, ok := b.resolveTicket(e)
	if !ok {
		return
	}

	if err := b.svc.Unclaim(ctx, actor, ticketID); err != nil {
		b.replyErr(e, b.closeErrMsg(err))
		return
	}

	slog.Info("ticket.unclaim", "ticket_id", ticketID, "user", actor)
	msgs := b.Msgs()
	msg := "Ticket unclaimed."
	if msgs != nil {
		msg = msgs.Ticket.Unclaimed
	}
	_ = e.CreateMessage(discord.MessageCreate{Content: msg})
}

func (b *Bot) handleAddUser(e *events.ApplicationCommandInteractionCreate) {
	ctx, actor, ticketID, ok := b.resolveTicket(e)
	if !ok {
		return
	}
	channelID := e.Channel().ID()
	target := e.SlashCommandInteractionData().User("user")

	if err := b.svc.AddUser(ctx, actor, ticketID, target.ID.String()); err != nil {
		b.replyErr(e, b.closeErrMsg(err))
		return
	}

	if err := b.addUserToChannel(channelID, target.ID); err != nil {
		slog.Error("add user to channel", "ticket", ticketID, "user", target.ID, "err", err)
		b.replyErr(e, messages.Format(b.Msgs().Ticket.UserAddPermFailed, "user", fmt.Sprintf("<@%s>", target.ID)))
		return
	}

	msgs := b.Msgs()
	msg := fmt.Sprintf("<@%s> added to ticket.", target.ID)
	if msgs != nil {
		msg = messages.Format(msgs.Ticket.UserAdded, "user", fmt.Sprintf("<@%s>", target.ID))
	}
	_ = e.CreateMessage(discord.MessageCreate{Content: msg})
}

func (b *Bot) handleRemoveUser(e *events.ApplicationCommandInteractionCreate) {
	ctx, actor, ticketID, ok := b.resolveTicket(e)
	if !ok {
		return
	}
	channelID := e.Channel().ID()
	target := e.SlashCommandInteractionData().User("user")

	if err := b.svc.RemoveUser(ctx, actor, ticketID, target.ID.String()); err != nil {
		b.replyErr(e, b.closeErrMsg(err))
		return
	}

	if err := b.removeUserFromChannel(channelID, target.ID); err != nil {
		slog.Error("remove user from channel", "ticket", ticketID, "user", target.ID, "err", err)
		b.replyErr(e, messages.Format(b.Msgs().Ticket.UserRemovePermFailed, "user", fmt.Sprintf("<@%s>", target.ID)))
		return
	}

	msgs := b.Msgs()
	msg := fmt.Sprintf("<@%s> removed from ticket.", target.ID)
	if msgs != nil {
		msg = messages.Format(msgs.Ticket.UserRemoved, "user", fmt.Sprintf("<@%s>", target.ID))
	}
	_ = e.CreateMessage(discord.MessageCreate{Content: msg})
}

func (b *Bot) handleSetStatus(e *events.ApplicationCommandInteractionCreate) {
	ctx, actor, ticketID, ok := b.resolveTicket(e)
	if !ok {
		return
	}
	statusStr := e.SlashCommandInteractionData().String("status")

	if err := b.svc.SetStatus(ctx, actor, ticketID, core.Status(statusStr)); err != nil {
		b.replyErr(e, b.closeErrMsg(err))
		return
	}

	msgs := b.Msgs()
	msg := fmt.Sprintf("Status set to %s.", statusStr)
	if msgs != nil {
		msg = messages.Format(msgs.Ticket.StatusSet, "status", statusStr)
	}
	_ = e.CreateMessage(discord.MessageCreate{Content: msg})
}

var dayWeekUnit = regexp.MustCompile(`([0-9.]*)([wd])`)

func parseDelay(s string) (time.Duration, bool) {
	compact := strings.ReplaceAll(s, " ", "")

	var extra time.Duration
	valid := true
	rest := dayWeekUnit.ReplaceAllStringFunc(compact, func(m string) string {
		sub := dayWeekUnit.FindStringSubmatch(m)
		n, err := strconv.ParseFloat(sub[1], 64)
		if err != nil {
			valid = false
			return ""
		}
		unit := 24 * time.Hour
		if sub[2] == "w" {
			unit = 7 * 24 * time.Hour
		}
		extra += time.Duration(float64(unit) * n)
		return ""
	})
	if !valid {
		return 0, false
	}

	var d time.Duration
	if rest != "" {
		var err error
		if d, err = time.ParseDuration(rest); err != nil {
			return 0, false
		}
	}
	total := d + extra
	if total <= 0 {
		return 0, false
	}
	return total, true
}

func (b *Bot) resolveTicket(e *events.ApplicationCommandInteractionCreate) (ctx context.Context, actor string, ticketID core.TicketID, ok bool) {
	ctx = context.Background()
	actor = e.User().ID.String()
	var err error
	ticketID, err = b.ticketFromChannel(ctx, e.Channel().ID())
	if err != nil {
		msgs := b.Msgs()
		msg := "This channel is not a ticket."
		if msgs != nil {
			msg = msgs.Ticket.NotATicket
		}
		b.replyErr(e, msg)
		return
	}
	ok = true
	return
}

func (b *Bot) replyErr(e *events.ApplicationCommandInteractionCreate, msg string) {
	_ = e.CreateMessage(discord.MessageCreate{
		Content: msg,
		Flags:   discord.MessageFlagEphemeral,
	})
}

func (b *Bot) closeErrMsg(err error) string {
	msgs := b.Msgs()
	switch {
	case errors.Is(err, core.ErrForbidden):
		return b.ForbiddenMsg("do that")
	case errors.Is(err, core.ErrBadStatus):
		if msgs != nil {
			return msgs.Errors.BadStatus
		}
		return "Ticket is not in a valid state for this action."
	case errors.Is(err, core.ErrNotFound):
		if msgs != nil {
			return msgs.Errors.NotFound
		}
		return "Ticket not found."
	case errors.Is(err, core.ErrTicketLimitReached):
		if msgs != nil {
			return msgs.Errors.TicketLimit
		}
		return "Ticket limit reached."
	default:
		if msgs != nil {
			return msgs.Errors.Internal
		}
		return "An internal error occurred."
	}
}

func applied(what string, id core.TicketID, err error) bool {
	if err == nil {
		return true
	}
	var he *core.HookError
	if errors.As(err, &he) {
		slog.Error(what+": hook failed after the change was applied", "ticket", id, "err", err)
		return true
	}
	return false
}
