package discord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
	"git.aaronf86.tech/aaronf86/ticketbot/messages"
)

const categoryCapacity = 50

func (b *Bot) resolveCategory(ctx context.Context, guildID snowflake.ID, panelID string) (snowflake.ID, error) {
	slots, err := b.channelStore.LoadCategorySlots(ctx, panelID)
	if err != nil {
		return 0, err
	}
	if len(slots) == 0 {
		return b.categoryForPanel(ctx, panelID)
	}
	for _, sl := range slots {
		if sl.Count < categoryCapacity {
			id, err := snowflake.Parse(sl.CategoryID)
			if err != nil {
				return 0, fmt.Errorf("invalid category_id for key %q: %w", sl.Key, err)
			}
			return id, nil
		}
	}

	panelName := panelID
	if p, ok := b.svc.Panel(panelID); ok {
		panelName = p.Name
	}
	nextSlot := len(slots) + 1
	key := fmt.Sprintf("%s:%d", panelID, nextSlot)
	name := fmt.Sprintf("%s - %d", panelName, nextSlot)
	cat, err := b.client.Rest.CreateGuildChannel(guildID, discord.GuildCategoryChannelCreate{Name: name})
	if err != nil {
		return 0, fmt.Errorf("create overflow category %q: %w", name, err)
	}
	catID := cat.ID()
	if err := b.channelStore.InsertCategoryIfNotExists(ctx, key, guildID.String(), catID.String()); err != nil {
		return 0, fmt.Errorf("store overflow category: %w", err)
	}
	return catID, nil
}

func (b *Bot) categoryForPanel(ctx context.Context, panelID string) (snowflake.ID, error) {
	fallback := b.cfg.CategoryFallbackKeys
	if len(fallback) == 0 {
		fallback = []string{"default"}
	}
	keys := append([]string{panelID}, fallback...)
	for _, key := range keys {
		catID, err := b.channelStore.GetServerConfig(ctx, key)
		if err != nil {
			continue
		}
		id, err := snowflake.Parse(catID)
		if err != nil {
			return 0, fmt.Errorf("invalid category_id in server config for key %q: %w", key, err)
		}
		return id, nil
	}
	return 0, fmt.Errorf("no category configured for panel %q (tried %v)", panelID, keys)
}

func (b *Bot) openChannel(ctx context.Context, guildID snowflake.ID, panelID string, ticket *core.Ticket, _ snowflake.ID) (snowflake.ID, error) {
	name := fmt.Sprintf("ticket-%d", ticket.ID)

	categoryID, err := b.resolveCategory(ctx, guildID, panelID)
	if err != nil {
		return 0, err
	}

	ch, err := b.client.Rest.CreateGuildChannel(guildID, discord.GuildTextChannelCreate{
		Name:     name,
		Topic:    fmt.Sprintf("%d", ticket.ID),
		ParentID: categoryID,
	})
	if err != nil {
		return 0, fmt.Errorf("create channel: %w", err)
	}
	channelID := ch.ID()

	if err := b.channelStore.SetChannelTicket(ctx, channelID.String(), int64(ticket.ID), categoryID.String()); err != nil {
		return channelID, fmt.Errorf("store channel mapping: %w", err)
	}
	return channelID, nil
}

func (b *Bot) removeChannel(ctx context.Context, channelID snowflake.ID) {
	_ = b.channelStore.DeleteChannel(ctx, channelID.String())
	if err := b.client.Rest.DeleteChannel(channelID); err != nil {
		slog.Error("failed to delete ticket channel", "channel", channelID, "err", err)
	}
}

func (b *Bot) archiveAndClose(channelID snowflake.ID, ticket *core.Ticket, panelName string) {
	b.closing.Add(1)
	go func() {
		defer b.closing.Done()
		ctx := context.Background()

		htmlBytes, err := b.buildTranscriptRetry(ctx, channelID, ticket, panelName)
		if err == nil {
			err = b.channelStore.UpsertTranscript(ctx, int64(ticket.ID), string(htmlBytes))
		}
		if err != nil {
			slog.Error("transcript failed; keeping ticket channel", "ticket", ticket.ID, "channel", channelID, "err", err)
			_ = b.Post(channelID, &discord.MessageCreate{
				Content: b.Msgs().Ticket.TranscriptFailed,
			})
			return
		}

		if panel, _ := b.svc.Panel(ticket.PanelID); panel == nil || panel.DMTranscript {
			b.dmTranscriptToUsers(ctx, ticket, htmlBytes, panelName)
		}

		b.removeChannel(ctx, channelID)
	}()
}

func (b *Bot) buildTranscriptRetry(ctx context.Context, channelID snowflake.ID, ticket *core.Ticket, panelName string) ([]byte, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		var out []byte
		if out, err = b.buildTranscript(ctx, channelID, ticket, panelName); err == nil {
			return out, nil
		}
		slog.Warn("build transcript failed", "ticket", ticket.ID, "attempt", attempt+1, "err", err)
	}
	return nil, err
}

func (b *Bot) buildTranscript(_ context.Context, channelID snowflake.ID, ticket *core.Ticket, panelName string) ([]byte, error) {
	const batch = 100
	var pages [][]discord.Message
	page := b.client.Rest.GetMessagesPage(channelID, 0, batch)
	for page.Next() {
		pages = append(pages, page.Items)
	}
	if page.Err != nil && !errors.Is(page.Err, rest.ErrNoMorePages) {
		return nil, page.Err
	}

	var body strings.Builder
	var msgCount int
	for i := len(pages) - 1; i >= 0; i-- {
		p := pages[i]
		for j := len(p) - 1; j >= 0; j-- {
			writeTranscriptMessage(&body, &p[j])
			msgCount++
		}
	}

	return renderTranscriptHTML(ticket, panelName, msgCount, body.String()), nil
}

func (b *Bot) dmTranscriptToUsers(_ context.Context, ticket *core.Ticket, htmlBytes []byte, panelName string) {
	msgs := b.Msgs()
	var tmplMsg string
	if msgs != nil {
		tmplMsg = messages.Format(msgs.Ticket.TranscriptDM, "panel", panelName)
	}

	fileName := fmt.Sprintf("transcript-%d.html", ticket.ID)

	var wg sync.WaitGroup
	for _, userID := range ticket.Users {
		uid, err := snowflake.Parse(userID)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func(uid snowflake.ID, userID string) {
			defer wg.Done()
			dm, err := b.client.Rest.CreateDMChannel(uid)
			if err != nil {
				slog.Warn("could not open DM for transcript", "user", userID, "err", err)
				return
			}
			f := discord.NewFile(fileName, "", bytes.NewReader(htmlBytes))
			if _, err := b.client.Rest.CreateMessage(dm.ID(), discord.MessageCreate{
				Content: tmplMsg,
			}.WithFiles(f)); err != nil {
				slog.Warn("could not send transcript DM", "user", userID, "err", err)
			}
		}(uid, userID)
	}
	wg.Wait()
}

func writeTranscriptMessage(sb *strings.Builder, m *discord.Message) {
	initials := "?"
	if m.Author.Username != "" {
		initials = strings.ToUpper(string([]rune(m.Author.Username)[:1]))
	}
	ts := m.CreatedAt.Format("2006-01-02 15:04")
	fmt.Fprintf(sb,
		`<div class="msg"><div class="avatar">%s</div><div class="body"><div class="header"><span class="author">%s</span><span class="ts">%s</span></div><div class="content">%s</div></div></div>`,
		html.EscapeString(initials),
		html.EscapeString(m.Author.Username),
		html.EscapeString(ts),
		html.EscapeString(m.Content),
	)
}

func renderTranscriptHTML(ticket *core.Ticket, panelName string, msgCount int, body string) []byte {
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><title>Transcript</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#1e2124;color:#dcddde;font-family:"Helvetica Neue",Helvetica,Arial,sans-serif;font-size:14px;line-height:1.5;padding:24px}
h1{font-size:20px;color:#fff;margin-bottom:4px}
.meta{color:#72767d;font-size:12px;margin-bottom:24px}
.msg{display:flex;gap:12px;padding:4px 0;margin-bottom:2px}
.msg:hover{background:#2f3136;border-radius:4px}
.avatar{width:40px;height:40px;border-radius:50%;background:#36393f;flex-shrink:0;display:flex;align-items:center;justify-content:center;color:#72767d;font-size:16px;font-weight:700}
.body{flex:1;min-width:0}
.header{display:flex;align-items:baseline;gap:8px;margin-bottom:2px}
.author{color:#fff;font-weight:600}
.ts{color:#72767d;font-size:11px}
.content{color:#dcddde;word-break:break-word;white-space:pre-wrap}
</style></head><body>`)

	fmt.Fprintf(&sb, "<h1>%s Ticket</h1>\n", html.EscapeString(panelName))
	fmt.Fprintf(&sb, `<div class="meta">Ticket ID: %d &nbsp;·&nbsp; Opened: %s &nbsp;·&nbsp; Messages: %d</div>`,
		ticket.ID,
		ticket.CreatedAt.Format("2006-01-02 15:04 UTC"),
		msgCount,
	)
	sb.WriteString(body)
	sb.WriteString("</body></html>")
	return []byte(sb.String())
}

func (b *Bot) addUserToChannel(channelID, userID snowflake.ID) error {
	allow := discord.PermissionViewChannel | discord.PermissionSendMessages | discord.PermissionReadMessageHistory
	return b.client.Rest.UpdatePermissionOverwrite(channelID, userID, discord.MemberPermissionOverwriteUpdate{
		Allow: &allow,
	})
}

func (b *Bot) AddUserToChannel(channelID, userID snowflake.ID) error {
	return b.addUserToChannel(channelID, userID)
}

func (b *Bot) removeUserFromChannel(channelID, userID snowflake.ID) error {
	return b.client.Rest.DeletePermissionOverwrite(channelID, userID)
}

func (b *Bot) ticketFromChannel(ctx context.Context, channelID snowflake.ID) (core.TicketID, error) {
	id, err := b.channelStore.GetTicketByChannel(ctx, channelID.String())
	if err != nil {
		return 0, fmt.Errorf("no ticket for this channel")
	}
	return core.TicketID(id), nil
}
