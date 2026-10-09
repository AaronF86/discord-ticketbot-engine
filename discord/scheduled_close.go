package discord

import (
	"context"
	"log/slog"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"golang.org/x/sync/errgroup"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
)

func (b *Bot) scheduleForceClose(ctx context.Context, ticketID core.TicketID, channelID snowflake.ID, actor string, closeAt time.Time) (bool, error) {
	if b.scheduledCloseStore == nil {
		b.armCloseTimer(ticketID, channelID, actor, time.Until(closeAt))
		return true, nil
	}

	inserted, err := b.scheduledCloseStore.Insert(ctx, ScheduledCloseParams{
		TicketID:  int64(ticketID),
		ChannelID: channelID.String(),
		Actor:     actor,
		CloseAt:   closeAt,
	})
	if err != nil {
		return false, err
	}
	if !inserted {
		return false, nil
	}
	b.armCloseTimer(ticketID, channelID, actor, time.Until(closeAt))
	return true, nil
}

func (b *Bot) armCloseTimer(ticketID core.TicketID, channelID snowflake.ID, actor string, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	b.scheduledClosesMu.Lock()
	if old, ok := b.scheduledCloses[ticketID]; ok {
		old.Stop()
	}
	b.scheduledCloses[ticketID] = time.AfterFunc(delay, func() {
		b.executeScheduledClose(context.Background(), ticketID, channelID, actor)
	})
	b.scheduledClosesMu.Unlock()
}

func (b *Bot) executeScheduledClose(ctx context.Context, ticketID core.TicketID, channelID snowflake.ID, actor string) {
	b.scheduledClosesMu.Lock()
	if t, ok := b.scheduledCloses[ticketID]; ok {
		t.Stop()
		delete(b.scheduledCloses, ticketID)
	}
	b.scheduledClosesMu.Unlock()

	if b.scheduledCloseStore != nil {
		deleted, err := b.scheduledCloseStore.Delete(ctx, int64(ticketID))
		if err != nil {
			slog.Error("scheduled_close: delete row", "ticket_id", ticketID, "err", err)
			return
		}
		if !deleted {
			return
		}
	}

	if err := b.svc.ForceClose(ctx, actor, ticketID); !applied("scheduled_close", ticketID, err) {
		slog.Error("scheduled_close: force close failed", "ticket_id", ticketID, "err", err)
		return
	}

	ticket, panelName := b.ticketAndPanel(ctx, ticketID)
	slog.Info("ticket.force_close.scheduled", "ticket_id", ticketID, "user", actor)
	b.archiveAndClose(channelID, ticket, panelName)
}

func (b *Bot) LoadScheduledCloses(ctx context.Context) error {
	if b.scheduledCloseStore == nil {
		return nil
	}
	closes, err := b.scheduledCloseStore.List(ctx)
	if err != nil {
		return err
	}

	var count int
	for _, sc := range closes {
		channelID, err := snowflake.Parse(sc.ChannelID)
		if err != nil {
			slog.Warn("scheduled_close: skipping row with invalid channel_id",
				"ticket_id", sc.TicketID, "channel_id", sc.ChannelID)
			continue
		}
		b.armCloseTimer(core.TicketID(sc.TicketID), channelID, sc.Actor, time.Until(sc.CloseAt))
		count++
	}
	slog.Info("scheduled closes loaded", "count", count)
	return nil
}

func (b *Bot) StartScheduledCloseWorker(ctx context.Context) {
	runEvery(ctx, 30*time.Second, b.processOverdueCloses)
}

type overdueClose struct {
	ticketID  core.TicketID
	channelID snowflake.ID
	actor     string
}

func (b *Bot) processOverdueCloses(ctx context.Context) {
	if b.scheduledCloseStore == nil {
		return
	}
	rows, err := b.scheduledCloseStore.ListOverdue(ctx)
	if err != nil {
		slog.Error("scheduled_close: query overdue", "err", err)
		return
	}

	var overdue []overdueClose
	for _, r := range rows {
		channelID, err := snowflake.Parse(r.ChannelID)
		if err != nil {
			continue
		}
		overdue = append(overdue, overdueClose{
			ticketID:  core.TicketID(r.TicketID),
			channelID: channelID,
			actor:     r.Actor,
		})
	}

	if len(overdue) == 0 {
		return
	}

	g, _ := errgroup.WithContext(context.Background())
	g.SetLimit(8)
	for _, oc := range overdue {
		g.Go(func() error {
			b.executeScheduledClose(context.Background(), oc.ticketID, oc.channelID, oc.actor)
			return nil
		})
	}
	_ = g.Wait()
}

func runEvery(ctx context.Context, d time.Duration, fn func(context.Context)) {
	go func() {
		ticker := time.NewTicker(d)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fn(ctx)
			}
		}
	}()
}
