package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
)

type AuditEvent struct {
	Actor    string
	Action   string
	TicketID int64
	Target   string
	Meta     map[string]any
}

type Formatter func(AuditEvent) string

type AuditLogger struct {
	pool *pgxpool.Pool

	mu      sync.RWMutex
	formats map[string]Formatter
}

func NewAuditLogger(pool *pgxpool.Pool) *AuditLogger {
	a := &AuditLogger{pool: pool, formats: make(map[string]Formatter)}
	for action, f := range defaultFormats() {
		a.formats[action] = f
	}
	return a
}

func (a *AuditLogger) RegisterFormat(action string, f Formatter) {
	a.mu.Lock()
	a.formats[action] = f
	a.mu.Unlock()
}

func (a *AuditLogger) Hooks() core.Hooks {
	return core.Hooks{
		AfterOpen: func(ctx context.Context, t *core.Ticket) error {
			return a.Log(ctx, t.CreatedBy, "ticket.opened", int64(t.ID), "", map[string]any{"panel_id": t.PanelID})
		},
		AfterClose: func(ctx context.Context, actor string, id core.TicketID) error {
			return a.Log(ctx, actor, "ticket.closed", int64(id), "", nil)
		},
		AfterForceClose: func(ctx context.Context, actor string, id core.TicketID) error {
			return a.Log(ctx, actor, "ticket.force_closed", int64(id), "", nil)
		},
		AfterAddUser: func(ctx context.Context, actor string, id core.TicketID, userID string) error {
			return a.Log(ctx, actor, "ticket.user_added", int64(id), userID, nil)
		},
		AfterRemoveUser: func(ctx context.Context, actor string, id core.TicketID, userID string) error {
			return a.Log(ctx, actor, "ticket.user_removed", int64(id), userID, nil)
		},
		AfterClaim: func(ctx context.Context, actor string, id core.TicketID) error {
			return a.Log(ctx, actor, "ticket.claimed", int64(id), "", nil)
		},
		AfterUnclaim: func(ctx context.Context, actor string, id core.TicketID) error {
			return a.Log(ctx, actor, "ticket.unclaimed", int64(id), "", nil)
		},
		AfterSetStatus: func(ctx context.Context, actor string, id core.TicketID, status core.Status) error {
			return a.Log(ctx, actor, "ticket.status_set", int64(id), "", map[string]any{"status": string(status)})
		},
		AfterRequestClose: func(ctx context.Context, actor string, id core.TicketID) error {
			return a.Log(ctx, actor, "ticket.close_requested", int64(id), "", nil)
		},
		AfterConfirmClose: func(ctx context.Context, actor string, id core.TicketID) error {
			return a.Log(ctx, actor, "ticket.close_confirmed", int64(id), "", nil)
		},
		AfterSetMetrics: func(ctx context.Context, actor string, id core.TicketID, metrics map[string]any) error {
			keys := make([]string, 0, len(metrics))
			for k := range metrics {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			values := make(map[string]string, len(metrics))
			for k, v := range metrics {
				values[k] = fmt.Sprintf("%v", v)
			}
			return a.Log(ctx, actor, "ticket.metrics_updated", int64(id), "", map[string]any{
				"keys":   strings.Join(keys, ", "),
				"values": values,
			})
		},
	}
}

func (a *AuditLogger) Log(ctx context.Context, actor, action string, ticketID int64, target string, meta map[string]any) error {
	metaBytes := []byte("{}")
	if meta != nil {
		b, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		metaBytes = b
	}

	var tid pgtype.Int8
	if ticketID != 0 {
		tid = pgtype.Int8{Int64: ticketID, Valid: true}
	}

	content := a.format(AuditEvent{Actor: actor, Action: action, TicketID: ticketID, Target: target, Meta: meta})

	return withTx(ctx, a.pool, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO audit_logs (actor, action, ticket_id, target, metadata)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, actor, action, tid, target, metaBytes).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO audit_broadcast_queue (audit_log_id, content) VALUES ($1, $2)`, id, content)
		return err
	})
}

func (a *AuditLogger) format(e AuditEvent) string {
	a.mu.RLock()
	f, ok := a.formats[e.Action]
	a.mu.RUnlock()
	if ok {
		return f(e)
	}
	mention := "<@" + e.Actor + ">"
	if e.TicketID != 0 {
		return fmt.Sprintf("Ticket #%d: **%s** by %s", e.TicketID, e.Action, mention)
	}
	return fmt.Sprintf("**%s** by %s", e.Action, mention)
}

func defaultFormats() map[string]Formatter {
	mention := func(e AuditEvent) string { return "<@" + e.Actor + ">" }
	str := func(e AuditEvent, key string) string { s, _ := e.Meta[key].(string); return s }

	return map[string]Formatter{
		"ticket.opened": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d opened by %s on panel **%s**", e.TicketID, mention(e), str(e, "panel_id"))
		},
		"ticket.closed": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d closed by %s", e.TicketID, mention(e))
		},
		"ticket.force_closed": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d force-closed by %s", e.TicketID, mention(e))
		},
		"ticket.claimed": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d claimed by %s", e.TicketID, mention(e))
		},
		"ticket.unclaimed": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d unclaimed by %s", e.TicketID, mention(e))
		},
		"ticket.user_added": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d: %s added <@%s>", e.TicketID, mention(e), e.Target)
		},
		"ticket.user_removed": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d: %s removed <@%s>", e.TicketID, mention(e), e.Target)
		},
		"ticket.status_set": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d status set to **%s** by %s", e.TicketID, str(e, "status"), mention(e))
		},
		"ticket.close_requested": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d: close requested by %s", e.TicketID, mention(e))
		},
		"ticket.close_confirmed": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d: close confirmed by %s", e.TicketID, mention(e))
		},
		"message.edited": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d: %s edited message %s", e.TicketID, mention(e), str(e, "message_id"))
		},
		"ticket.metrics_updated": func(e AuditEvent) string {
			return fmt.Sprintf("Ticket #%d: metrics updated (%s) by %s", e.TicketID, str(e, "keys"), mention(e))
		},
	}
}
