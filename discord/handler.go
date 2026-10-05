package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
	"git.aaronf86.tech/aaronf86/ticketbot/messages"
)

func (b *Bot) handleOpen(e *events.ApplicationCommandInteractionCreate) {
	panelID := e.SlashCommandInteractionData().String("panel")

	if fn, ok := b.panelModals[panelID]; ok {
		_ = e.Modal(fn(0))
		return
	}

	ctx := context.Background()
	actor := e.User().ID
	guildID := *e.GuildID()

	result, err := b.CreateTicket(ctx, guildID, panelID, actor.String())
	if err != nil {
		b.replyErr(e, fmt.Sprintf("Failed to open ticket: %v", err))
		return
	}

	slog.Info("ticket.open", "ticket_id", result.TicketID, "panel", panelID, "user", actor.String(), "channel", result.ChannelID.String())
	msgs := b.Msgs()
	msg := fmt.Sprintf("Ticket created: <#%s>", result.ChannelID)
	if msgs != nil {
		msg = messages.Format(msgs.Ticket.OpenSuccess, "channel", result.ChannelID.String())
	}
	_ = e.CreateMessage(discord.MessageCreate{
		Content: msg,
		Flags:   discord.MessageFlagEphemeral,
	})
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

	if err := b.svc.Close(ctx, actor, ticketID); err != nil {
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

	delayStr, hasDelay := e.SlashCommandInteractionData().OptString("delay")
	if !hasDelay || delayStr == "" {
		if err := b.svc.ForceClose(ctx, actor, ticketID); err != nil {
			b.replyErr(e, b.closeErrMsg(err))
			return
		}
		ticket, panelName := b.ticketAndPanel(ctx, ticketID)
		slog.Info("ticket.force_close", "ticket_id", ticketID, "user", actor)
		_ = e.CreateMessage(discord.MessageCreate{Content: b.forceCloseSuccessMsg(ticket.PanelID)})
		b.archiveAndClose(channelID, ticket, panelName)
		return
	}

	delay, ok := parseDelay(delayStr)
	if !ok {
		msgs := b.Msgs()
		msg := "Invalid delay format."
		if msgs != nil {
			msg = msgs.Ticket.ForceCloseInvalidDelay
		}
		b.replyErr(e, msg)
		return
	}

	closeAt := time.Now().Add(delay)
	scheduled, err := b.scheduleForceClose(ctx, ticketID, channelID, actor, closeAt)
	if err != nil {
		slog.Error("schedule force close", "err", err)
		msgs := b.Msgs()
		msg := "Failed to schedule close."
		if msgs != nil {
			msg = msgs.Errors.Internal
		}
		b.replyErr(e, msg)
		return
	}
	if !scheduled {
		msgs := b.Msgs()
		msg := "A close is already scheduled."
		if msgs != nil {
			msg = msgs.Ticket.ForceCloseAlreadyScheduled
		}
		b.replyErr(e, msg)
		return
	}

	ts := fmt.Sprintf("<t:%d:R>", closeAt.Unix())
	msgs := b.Msgs()
	msg := fmt.Sprintf("Force close scheduled for %s.", ts)
	if msgs != nil {
		msg = messages.Format(msgs.Ticket.ForceCloseScheduled, "time", ts)
	}
	_ = e.CreateMessage(discord.MessageCreate{Content: msg})
}

func (b *Bot) handleTimedClose(e *events.ApplicationCommandInteractionCreate) {
	ctx, actor, ticketID, ok := b.resolveTicket(e)
	if !ok {
		return
	}
	channelID := e.Channel().ID()

	allowed, _ := b.auth.Allow(ctx, actor, core.ActionForceClose, ticketID)
	if !allowed {
		b.replyErr(e, b.ForbiddenMsg("do that"))
		return
	}

	delay := 24 * time.Hour
	if delayStr, ok := e.SlashCommandInteractionData().OptString("delay"); ok && delayStr != "" {
		d, ok := parseDelay(delayStr)
		if !ok {
			msgs := b.Msgs()
			msg := "Invalid delay format."
			if msgs != nil {
				msg = msgs.TimedClose.InvalidDelay
			}
			b.replyErr(e, msg)
			return
		}
		delay = d
	}

	ticket, err := b.svc.Get(ctx, ticketID)
	if err != nil {
		b.replyErr(e, b.Msgs().Errors.Internal)
		return
	}

	closeAt := time.Now().Add(delay)
	scheduled, err := b.scheduleTimedClose(ctx, ticketID, channelID, actor, ticket.CreatedBy, closeAt)
	if err != nil {
		slog.Error("schedule timed close", "err", err)
		msgs := b.Msgs()
		msg := "Failed to schedule timed close."
		if msgs != nil {
			msg = msgs.Errors.Internal
		}
		b.replyErr(e, msg)
		return
	}
	if !scheduled {
		msgs := b.Msgs()
		msg := "A timed close is already scheduled."
		if msgs != nil {
			msg = msgs.TimedClose.AlreadyScheduled
		}
		b.replyErr(e, msg)
		return
	}

	_ = e.CreateMessage(MsgEphemeral("✓"))
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
		b.replyErr(e, fmt.Sprintf("User added to ticket but channel permission failed: %v", err))
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
		b.replyErr(e, fmt.Sprintf("User removed from ticket but channel permission failed: %v", err))
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

func parseDelay(s string) (time.Duration, bool) {
	compact := strings.ReplaceAll(s, " ", "")

	var extra time.Duration
	for _, pair := range []struct {
		suffix string
		unit   time.Duration
	}{
		{"w", 7 * 24 * time.Hour},
		{"d", 24 * time.Hour},
	} {
		for strings.Contains(compact, pair.suffix) {
			idx := strings.Index(compact, pair.suffix)
			start := idx - 1
			for start > 0 && compact[start-1] >= '0' && compact[start-1] <= '9' {
				start--
			}
			n, err := strconv.ParseFloat(compact[start:idx], 64)
			if err != nil {
				return 0, false
			}
			extra += time.Duration(float64(pair.unit) * n)
			compact = compact[:start] + compact[idx+len(pair.suffix):]
		}
	}

	var d time.Duration
	if compact != "" {
		var err error
		d, err = time.ParseDuration(compact)
		if err != nil {
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
