package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
	"git.aaronf86.tech/aaronf86/ticketbot/messages"
)

const prefixTCKeepOpen = "tc:keep_open:"

type timedCloseEntry struct {
	timer     *time.Timer
	messageID snowflake.ID
	channelID snowflake.ID
	createdBy string
}

func (b *Bot) scheduleTimedClose(ctx context.Context, ticketID core.TicketID, channelID snowflake.ID, actor, createdBy string, closeAt time.Time) (bool, error) {
	ts := fmt.Sprintf("<t:%d:R>", closeAt.Unix())
	msgs := b.Msgs()
	content := fmt.Sprintf("This ticket will be closed %s.", ts)
	if msgs != nil {
		content = messages.Format(msgs.TimedClose.Scheduled, "time", ts)
	}

	id := strconv.FormatInt(int64(ticketID), 10)
	msg, err := b.PostMessage(channelID, discord.MessageCreate{
		Content: content,
		Components: []discord.LayoutComponent{
			discord.NewActionRow(
				b.BuildButton(ctx, ButtonKeepOpen, prefixTCKeepOpen+id),
			),
		},
	})
	if err != nil {
		return false, err
	}

	if b.timedCloseStore == nil {
		b.armTimedCloseTimer(ticketID, channelID, msg.ID, actor, createdBy, time.Until(closeAt))
		return true, nil
	}

	inserted, err := b.timedCloseStore.Insert(ctx, TimedCloseParams{
		TicketID:  int64(ticketID),
		ChannelID: channelID.String(),
		Actor:     actor,
		CreatedBy: createdBy,
		MessageID: msg.ID.String(),
		CloseAt:   closeAt,
	})
	if err != nil {
		_ = b.client.Rest.DeleteMessage(channelID, msg.ID)
		return false, err
	}
	if !inserted {
		_ = b.client.Rest.DeleteMessage(channelID, msg.ID)
		return false, nil
	}

	b.armTimedCloseTimer(ticketID, channelID, msg.ID, actor, createdBy, time.Until(closeAt))

	if b.auditLog != nil {
		_ = b.auditLog(ctx, actor, "ticket.timed_close_scheduled", int64(ticketID), "", map[string]any{
			"close_at": closeAt.UTC().Format(time.RFC3339),
		})
	}
	return true, nil
}

func (b *Bot) armTimedCloseTimer(ticketID core.TicketID, channelID, msgID snowflake.ID, actor, createdBy string, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	b.timedClosesMu.Lock()
	if old, ok := b.timedCloses[ticketID]; ok {
		old.timer.Stop()
	}
	entry := &timedCloseEntry{
		messageID: msgID,
		channelID: channelID,
		createdBy: createdBy,
	}
	entry.timer = time.AfterFunc(delay, func() {
		b.executeTimedClose(context.Background(), ticketID, channelID, actor)
	})
	b.timedCloses[ticketID] = entry
	b.timedClosesMu.Unlock()
}

func (b *Bot) cancelTimedClose(ctx context.Context, ticketID core.TicketID, actor, cancelMsg string) {
	b.timedClosesMu.Lock()
	entry, ok := b.timedCloses[ticketID]
	if ok {
		entry.timer.Stop()
		delete(b.timedCloses, ticketID)
	}
	b.timedClosesMu.Unlock()

	if b.timedCloseStore != nil {
		if _, err := b.timedCloseStore.Delete(ctx, int64(ticketID)); err != nil {
			slog.Error("timed_close: cancel delete", "ticket_id", ticketID, "err", err)
		}
	}

	if ok && entry.messageID != 0 {
		update := discord.NewMessageUpdate()
		update.Content = &cancelMsg
		empty := []discord.LayoutComponent{}
		update.Components = &empty
		_, _ = b.client.Rest.UpdateMessage(entry.channelID, entry.messageID, update)
	}

	if b.auditLog != nil {
		_ = b.auditLog(ctx, actor, "ticket.timed_close_cancelled", int64(ticketID), "", nil)
	}
}

func (b *Bot) MaybeCancelTimedClose(ctx context.Context, ticketID core.TicketID, authorID string) {
	b.timedClosesMu.RLock()
	entry, ok := b.timedCloses[ticketID]
	b.timedClosesMu.RUnlock()

	if !ok || entry.createdBy != authorID {
		return
	}

	mention := fmt.Sprintf("<@%s>", authorID)
	msgs := b.Msgs()
	cancelMsg := fmt.Sprintf("%s responded — timed close cancelled.", mention)
	if msgs != nil {
		cancelMsg = messages.Format(msgs.TimedClose.CancelledMessage, "user", mention)
	}
	b.cancelTimedClose(ctx, ticketID, authorID, cancelMsg)
}

func (b *Bot) executeTimedClose(ctx context.Context, ticketID core.TicketID, channelID snowflake.ID, actor string) {
	b.timedClosesMu.Lock()
	if t, ok := b.timedCloses[ticketID]; ok {
		t.timer.Stop()
		delete(b.timedCloses, ticketID)
	}
	b.timedClosesMu.Unlock()

	if b.timedCloseStore != nil {
		deleted, err := b.timedCloseStore.Delete(ctx, int64(ticketID))
		if err != nil {
			slog.Error("timed_close: execute delete", "ticket_id", ticketID, "err", err)
			return
		}
		if !deleted {
			return
		}
	}

	if err := b.svc.ForceClose(ctx, actor, ticketID); err != nil {
		slog.Error("timed_close: force close failed", "ticket_id", ticketID, "err", err)
		return
	}

	if b.auditLog != nil {
		_ = b.auditLog(ctx, actor, "ticket.timed_close_executed", int64(ticketID), "", nil)
	}

	ticket, panelName := b.ticketAndPanel(ctx, ticketID)
	slog.Info("ticket.timed_close", "ticket_id", ticketID, "user", actor)
	b.archiveAndClose(channelID, ticket, panelName)
}

func (b *Bot) LoadTimedCloses(ctx context.Context) error {
	if b.timedCloseStore == nil {
		return nil
	}
	closes, err := b.timedCloseStore.List(ctx)
	if err != nil {
		return err
	}

	var count int
	for _, tc := range closes {
		channelID, err := snowflake.Parse(tc.ChannelID)
		if err != nil {
			slog.Warn("timed_close: skipping row with invalid channel_id", "ticket_id", tc.TicketID)
			continue
		}
		msgID, _ := snowflake.Parse(tc.MessageID)
		b.armTimedCloseTimer(core.TicketID(tc.TicketID), channelID, msgID, tc.Actor, tc.CreatedBy, time.Until(tc.CloseAt))
		count++
	}
	slog.Info("timed closes loaded", "count", count)
	return nil
}

func (b *Bot) StartTimedCloseWorker(ctx context.Context) {
	runEvery(ctx, 30*time.Second, b.processOverdueTimedCloses)
}

func (b *Bot) processOverdueTimedCloses(ctx context.Context) {
	if b.timedCloseStore == nil {
		return
	}
	overdue, err := b.timedCloseStore.ListOverdue(ctx)
	if err != nil {
		slog.Error("timed_close: query overdue", "err", err)
		return
	}
	if len(overdue) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, oc := range overdue {
		channelID, err := snowflake.Parse(oc.ChannelID)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func(ticketID core.TicketID, channelID snowflake.ID, actor string) {
			defer wg.Done()
			b.executeTimedClose(context.Background(), ticketID, channelID, actor)
		}(core.TicketID(oc.TicketID), channelID, oc.Actor)
	}
	wg.Wait()
}

type TimedCloseKeepOpenHandler struct{ bot *Bot }

func NewTimedCloseKeepOpenHandler(bot *Bot) *TimedCloseKeepOpenHandler {
	return &TimedCloseKeepOpenHandler{bot: bot}
}

func (h *TimedCloseKeepOpenHandler) Prefix() string { return prefixTCKeepOpen }

func (h *TimedCloseKeepOpenHandler) Handle(ctx context.Context, e *events.ComponentInteractionCreate) error {
	msgs := h.bot.Msgs()
	n, err := strconv.ParseInt(strings.TrimPrefix(e.Data.CustomID(), prefixTCKeepOpen), 10, 64)
	if err != nil {
		msg := "Invalid ID."
		if msgs != nil {
			msg = msgs.Errors.InvalidID
		}
		return e.CreateMessage(MsgEphemeral(msg))
	}
	ticketID := core.TicketID(n)
	actorID := e.User().ID.String()

	ticket, err := h.bot.svc.Get(ctx, ticketID)
	if err != nil {
		msg := "An internal error occurred."
		if msgs != nil {
			msg = msgs.Errors.Internal
		}
		return e.CreateMessage(MsgEphemeral(msg))
	}
	isOwner := actorID == ticket.CreatedBy
	isStaff, _ := h.bot.auth.Allow(ctx, actorID, core.ActionClose, ticketID)
	if !isOwner && !isStaff {
		msg := "Permission denied."
		if msgs != nil {
			msg = msgs.Errors.Forbidden
		}
		return e.CreateMessage(MsgEphemeral(msg))
	}

	mention := fmt.Sprintf("<@%s>", e.User().ID)
	cancelMsg := fmt.Sprintf("%s kept the ticket open.", mention)
	if msgs != nil {
		cancelMsg = messages.Format(msgs.TimedClose.CancelledButton, "user", mention)
	}
	h.bot.cancelTimedClose(ctx, ticketID, actorID, cancelMsg)

	return e.DeferUpdateMessage()
}
