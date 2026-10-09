package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
)

type Store struct {
	pool *pgxpool.Pool
}

var _ core.Store = (*Store)(nil)

func NewStore(ctx context.Context, connString string) (*Store, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func NewStoreFromPool(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Close()              { s.pool.Close() }
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

const ticketSelect = `SELECT t.id, t.panel_id, t.status, t.created_at, t.last_message, t.left_on_read, t.created_by,
	t.claimed_by, t.close_requested_by, t.workflow_completed_steps,
	t.first_staff_reply_at, t.wait_started_at, t.total_wait_secs, t.version,
	ARRAY(SELECT tu.user_id FROM ticket_users tu WHERE tu.ticket_id = t.id ORDER BY tu.added_at, tu.user_id),
	COALESCE((SELECT jsonb_object_agg(m.key, m.value) FROM ticket_meta m WHERE m.ticket_id = t.id), '{}'::jsonb)
	FROM tickets t`

func scanTicket(row pgx.Row) (*core.Ticket, error) {
	var (
		id                              int64
		panelID, status                 string
		createdAt                       time.Time
		lastMessage, firstReply, waitAt pgtype.Timestamptz
		leftOnRead                      bool
		createdBy, claimedBy, closeReq  string
		steps, users                    []string
		totalWaitSecs                   float64
		version                         int64
		meta                            map[string]string
	)
	if err := row.Scan(&id, &panelID, &status, &createdAt, &lastMessage, &leftOnRead, &createdBy,
		&claimedBy, &closeReq, &steps, &firstReply, &waitAt, &totalWaitSecs, &version, &users, &meta); err != nil {
		return nil, err
	}
	if steps == nil {
		steps = []string{}
	}
	if users == nil {
		users = []string{}
	}
	if meta == nil {
		meta = map[string]string{}
	}
	return &core.Ticket{
		ID:                core.TicketID(id),
		PanelID:           panelID,
		Status:            core.Status(status),
		CreatedAt:         createdAt,
		LastMessage:       fromNullTime(lastMessage),
		LeftOnRead:        leftOnRead,
		CreatedBy:         createdBy,
		ClaimedBy:         claimedBy,
		CloseRequestedBy:  closeReq,
		Users:             users,
		Meta:              meta,
		Version:           version,
		FirstStaffReplyAt: fromNullTime(firstReply),
		WaitStartedAt:     fromNullTime(waitAt),
		TotalWaitSecs:     int64(totalWaitSecs),
		WorkflowState:     core.WorkflowState{CompletedSteps: steps},
	}, nil
}

func (s *Store) Create(ctx context.Context, t *core.Ticket, limit int) error {
	t.CreatedAt = time.Now().UTC()
	return withTx(ctx, s.pool, func(tx pgx.Tx) error {
		if limit > 0 {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
				t.CreatedBy+"|"+t.PanelID); err != nil {
				return fmt.Errorf("lock ticket limit: %w", err)
			}
			var open int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM tickets t
				WHERE t.panel_id = $2 AND t.status = 'open'
				  AND EXISTS (SELECT 1 FROM ticket_users tu WHERE tu.ticket_id = t.id AND tu.user_id = $1)`,
				t.CreatedBy, t.PanelID).Scan(&open); err != nil {
				return fmt.Errorf("count open tickets: %w", err)
			}
			if open >= limit {
				return core.ErrTicketLimitReached
			}
		}
		var id int64
		err := tx.QueryRow(ctx, `
			INSERT INTO tickets (
				panel_id, status, created_at, last_message, left_on_read, created_by, claimed_by,
				close_requested_by, workflow_completed_steps, first_staff_reply_at, wait_started_at, total_wait_secs
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id, version`,
			t.PanelID, string(t.Status), t.CreatedAt, toNullTime(t.LastMessage), t.LeftOnRead, t.CreatedBy,
			t.ClaimedBy, t.CloseRequestedBy, coalesceSlice(t.WorkflowState.CompletedSteps),
			toNullTime(t.FirstStaffReplyAt), toNullTime(t.WaitStartedAt), t.TotalWaitSecs,
		).Scan(&id, &t.Version)
		if err != nil {
			return fmt.Errorf("create ticket: %w", err)
		}
		t.ID = core.TicketID(id)
		return syncAssociations(ctx, tx, t)
	})
}

func (s *Store) Get(ctx context.Context, id core.TicketID) (*core.Ticket, error) {
	t, err := scanTicket(s.pool.QueryRow(ctx, ticketSelect+` WHERE t.id = $1`, int64(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	return t, err
}

func (s *Store) Update(ctx context.Context, t *core.Ticket) error {
	return withTx(ctx, s.pool, func(tx pgx.Tx) error {
		var newVersion int64
		err := tx.QueryRow(ctx, `
			UPDATE tickets SET
				status = $2, last_message = $3, left_on_read = $4, claimed_by = $5,
				close_requested_by = $6, workflow_completed_steps = $7,
				first_staff_reply_at = $8, wait_started_at = $9, total_wait_secs = $10,
				version = version + 1
			WHERE id = $1 AND version = $11
			RETURNING version`,
			int64(t.ID), string(t.Status), toNullTime(t.LastMessage), t.LeftOnRead, t.ClaimedBy,
			t.CloseRequestedBy, coalesceSlice(t.WorkflowState.CompletedSteps),
			toNullTime(t.FirstStaffReplyAt), toNullTime(t.WaitStartedAt), t.TotalWaitSecs, t.Version,
		).Scan(&newVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if qerr := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tickets WHERE id = $1)`, int64(t.ID)).Scan(&exists); qerr != nil {
				return qerr
			}
			if !exists {
				return core.ErrNotFound
			}
			return core.ErrConflict
		}
		if err != nil {
			return fmt.Errorf("update ticket: %w", err)
		}
		if err := syncAssociations(ctx, tx, t); err != nil {
			return err
		}
		t.Version = newVersion
		return nil
	})
}

func (s *Store) ListByPanel(ctx context.Context, panelID string) ([]*core.Ticket, error) {
	return s.list(ctx, ticketSelect+` WHERE t.panel_id = $1 ORDER BY t.created_at DESC`, panelID)
}

func (s *Store) ListByUser(ctx context.Context, userID string) ([]*core.Ticket, error) {
	return s.list(ctx, ticketSelect+`
		WHERE EXISTS (SELECT 1 FROM ticket_users tu WHERE tu.ticket_id = t.id AND tu.user_id = $1)
		ORDER BY t.created_at DESC`, userID)
}

func (s *Store) Archive(ctx context.Context, id core.TicketID) error {
	_, err := s.pool.Exec(ctx, `UPDATE tickets SET status = 'archived' WHERE id = $1`, int64(id))
	return err
}

const globalPanelKey = "__global__"

func (s *Store) PanelLimits(ctx context.Context, panelID string) (panel, global int, err error) {
	rows, err := s.pool.Query(ctx, `SELECT panel_id, max_tickets FROM panel_settings WHERE panel_id = ANY($1)`,
		[]string{panelID, globalPanelKey})
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var limit int32
		if err := rows.Scan(&id, &limit); err != nil {
			return 0, 0, err
		}
		if id == globalPanelKey {
			global = int(limit)
		} else {
			panel = int(limit)
		}
	}
	return panel, global, rows.Err()
}

func (s *Store) SetPanelLimit(ctx context.Context, panelID string, maxTickets int) error {
	if maxTickets < 0 || maxTickets > math.MaxInt32 {
		return fmt.Errorf("panel limit %d out of range", maxTickets)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO panel_settings (panel_id, max_tickets) VALUES ($1, $2)
		ON CONFLICT (panel_id) DO UPDATE SET max_tickets = EXCLUDED.max_tickets`, panelID, int32(maxTickets))
	return err
}

func (s *Store) UpsertMetric(ctx context.Context, id core.TicketID, key string, value any) error {
	kind := core.KindOf(value)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ticket_metrics (ticket_id, key, kind, value) VALUES ($1, $2, $3, $4)
		ON CONFLICT (ticket_id, key) DO UPDATE SET kind = EXCLUDED.kind, value = EXCLUDED.value`,
		int64(id), key, string(kind), serializeValue(value, kind))
	return err
}

func (s *Store) IncrementMetric(ctx context.Context, id core.TicketID, key string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ticket_metrics (ticket_id, key, kind, value) VALUES ($1, $2, 'int', '1')
		ON CONFLICT (ticket_id, key) DO UPDATE
		SET value = (ticket_metrics.value::BIGINT + 1)::TEXT`, int64(id), key)
	return err
}

func (s *Store) list(ctx context.Context, query string, arg any) ([]*core.Ticket, error) {
	rows, err := s.pool.Query(ctx, query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*core.Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func syncAssociations(ctx context.Context, tx pgx.Tx, t *core.Ticket) error {
	id := int64(t.ID)
	users := coalesceSlice(t.Users)
	if _, err := tx.Exec(ctx, `DELETE FROM ticket_users WHERE ticket_id = $1 AND user_id <> ALL($2::text[])`, id, users); err != nil {
		return fmt.Errorf("delete ticket users: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ticket_users (ticket_id, user_id)
		SELECT $1, u FROM unnest($2::text[]) AS u ON CONFLICT (ticket_id, user_id) DO NOTHING`, id, users); err != nil {
		return fmt.Errorf("upsert ticket users: %w", err)
	}

	keys := make([]string, 0, len(t.Meta))
	vals := make([]string, 0, len(t.Meta))
	for k, v := range t.Meta {
		keys = append(keys, k)
		vals = append(vals, v)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM ticket_meta WHERE ticket_id = $1 AND key <> ALL($2::text[])`, id, keys); err != nil {
		return fmt.Errorf("delete ticket meta: %w", err)
	}
	if len(keys) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO ticket_meta (ticket_id, key, value)
			SELECT $1, k, v FROM unnest($2::text[], $3::text[]) AS x(k, v)
			ON CONFLICT (ticket_id, key) DO UPDATE SET value = EXCLUDED.value
			WHERE ticket_meta.value IS DISTINCT FROM EXCLUDED.value`, id, keys, vals); err != nil {
			return fmt.Errorf("upsert ticket meta: %w", err)
		}
	}
	return nil
}

func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func coalesceSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func toNullTime(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func fromNullTime(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

func serializeValue(v any, kind core.MetricKind) string {
	switch kind {
	case core.MetricKindDuration:
		if d, ok := v.(time.Duration); ok {
			return strconv.FormatInt(int64(d), 10)
		}
	case core.MetricKindInt:
		if i, ok := v.(int); ok {
			return strconv.Itoa(i)
		}
	case core.MetricKindBool:
		if b, ok := v.(bool); ok {
			return strconv.FormatBool(b)
		}
	case core.MetricKindString:
		if s, ok := v.(string); ok {
			return s
		}
	}
	return fmt.Sprintf("%v", v)
}
