package discord

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

type UnansweredOptions struct {
	After       time.Duration
	Repeat      time.Duration
	Interval    time.Duration
	RoleMention func(ctx context.Context, guildID string) string
}

func (b *Bot) StartUnansweredWatcher(ctx context.Context, store UnansweredStore, opts UnansweredOptions) {
	if opts.After <= 0 {
		return
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Minute
	}
	w := &unansweredWatcher{b: b, store: store, opts: opts, warned: make(map[string]bool)}
	runEvery(ctx, opts.Interval, w.tick)
}

type unansweredWatcher struct {
	b      *Bot
	store  UnansweredStore
	opts   UnansweredOptions
	warned map[string]bool
}

func (w *unansweredWatcher) tick(ctx context.Context) {
	waiting, err := w.store.ListUnanswered(ctx, w.opts.After, w.opts.Repeat)
	if err != nil {
		slog.Error("unanswered: query", "err", err)
		return
	}
	for i := range waiting {
		w.alert(ctx, &waiting[i])
	}
}

func (w *unansweredWatcher) alert(ctx context.Context, t *UnansweredTicket) {
	guildID, ok := w.ticketGuild(t)
	if !ok {
		w.markAlerted(ctx, t)
		return
	}
	logID, ok := w.logChannel(ctx, guildID)
	if !ok {
		return
	}

	var roleID snowflake.ID
	if w.opts.RoleMention != nil {
		if r, err := snowflake.Parse(w.opts.RoleMention(ctx, guildID.String())); err == nil {
			roleID = r
		}
	}

	msg := discord.MessageCreate{
		Content:         UnansweredMessage(w.b.panelName(t.PanelID), t, time.Now(), roleID),
		AllowedMentions: &discord.AllowedMentions{Roles: rolesOf(roleID)},
	}
	if _, err := w.b.Rest().CreateMessage(logID, msg); err != nil {
		slog.Error("unanswered: post alert", "ticket", t.TicketID, "err", err)
		return
	}
	w.markAlerted(ctx, t)
}

func (w *unansweredWatcher) markAlerted(ctx context.Context, t *UnansweredTicket) {
	if err := w.store.MarkUnansweredAlerted(ctx, t.TicketID, t.WaitStartedAt); err != nil {
		slog.Error("unanswered: mark alerted", "ticket", t.TicketID, "err", err)
	}
}

func (w *unansweredWatcher) ticketGuild(t *UnansweredTicket) (guildID snowflake.ID, ok bool) {
	chID, err := snowflake.Parse(t.ChannelID)
	if err != nil {
		return 0, false
	}
	ch, err := w.b.Rest().GetChannel(chID)
	if err != nil {
		slog.Warn("unanswered: ticket channel lookup failed", "ticket", t.TicketID, "err", err)
		return 0, false
	}
	gc, isGuild := ch.(discord.GuildChannel)
	if !isGuild {
		return 0, false
	}
	return gc.GuildID(), true
}

func (w *unansweredWatcher) logChannel(ctx context.Context, guildID snowflake.ID) (id snowflake.ID, ok bool) {
	logCh, err := w.store.GuildLogChannel(ctx, guildID.String())
	if err != nil {
		slog.Error("unanswered: log channel lookup", "guild", guildID, "err", err)
		return 0, false
	}
	id, perr := snowflake.Parse(logCh)
	if logCh == "" || perr != nil {
		if !w.warned[guildID.String()] {
			slog.Warn("unanswered: guild has no valid log channel, alerts are skipped", "guild", guildID)
			w.warned[guildID.String()] = true
		}
		return 0, false
	}
	return id, true
}

func (b *Bot) panelName(id string) string {
	if p, ok := b.Service().Panel(id); ok && p.Name != "" {
		return p.Name
	}
	return id
}

func rolesOf(id snowflake.ID) []snowflake.ID {
	if id == 0 {
		return nil
	}
	return []snowflake.ID{id}
}

func UnansweredMessage(panelName string, t *UnansweredTicket, now time.Time, role snowflake.ID) string {
	waited := now.Sub(t.WaitStartedAt).Round(time.Minute)
	ping := ""
	if role != 0 {
		ping = fmt.Sprintf("<@&%s> ", role)
	}
	again := ""
	if t.Alerts > 0 {
		again = " (still unanswered)"
	}
	return fmt.Sprintf("%s Ticket #%d (<#%s>) on **%s** has had no staff reply for **%d min**%s. Opened by <@%s>.",
		ping, t.TicketID, t.ChannelID, panelName, int(waited.Minutes()), again, t.CreatedBy)
}
