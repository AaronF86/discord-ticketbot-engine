package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	tb "git.aaronf86.tech/aaronf86/ticketbot/discord"
)

type DiscordStores struct {
	pool *pgxpool.Pool
}

var (
	_ tb.ChannelStore     = (*DiscordStores)(nil)
	_ tb.GuildStore       = (*DiscordStores)(nil)
	_ tb.TimedCloseStore  = (*DiscordStores)(nil)
	_ tb.RoleSyncStore    = (*DiscordStores)(nil)
	_ tb.SentMessageStore = (*DiscordStores)(nil)
	_ tb.EmojiStore       = (*DiscordStores)(nil)
)

func NewDiscordStores(pool *pgxpool.Pool) *DiscordStores { return &DiscordStores{pool: pool} }

func (s *DiscordStores) SetChannelTicket(ctx context.Context, channelID string, ticketID int64, categoryID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO discord_channels (channel_id, ticket_id, category_id) VALUES ($1, $2, $3)
		ON CONFLICT (channel_id) DO UPDATE SET ticket_id = EXCLUDED.ticket_id, category_id = EXCLUDED.category_id`,
		channelID, ticketID, categoryID)
	return err
}

func (s *DiscordStores) GetTicketByChannel(ctx context.Context, channelID string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT ticket_id FROM discord_channels WHERE channel_id = $1`, channelID).Scan(&id)
	return id, err
}

func (s *DiscordStores) DeleteChannel(ctx context.Context, channelID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM discord_channels WHERE channel_id = $1`, channelID)
	return err
}

func (s *DiscordStores) ListActiveChannelIDsForUser(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT dc.channel_id
		FROM discord_channels dc
		JOIN tickets t ON t.id = dc.ticket_id
		WHERE t.status NOT IN ('closed', 'archived')
		  AND (t.created_by = $1
		       OR EXISTS (SELECT 1 FROM ticket_users tu WHERE tu.ticket_id = t.id AND tu.user_id = $1))`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *DiscordStores) GetServerConfig(ctx context.Context, key string) (string, error) {
	var guildID, categoryID string
	err := s.pool.QueryRow(ctx, `SELECT guild_id, category_id FROM discord_server_config WHERE key = $1 AND active = TRUE`, key).
		Scan(&guildID, &categoryID)
	return categoryID, err
}

func (s *DiscordStores) InsertCategoryIfNotExists(ctx context.Context, key, guildID, categoryID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO discord_server_config (key, guild_id, category_id) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO NOTHING`, key, guildID, categoryID)
	return err
}

func (s *DiscordStores) LoadCategorySlots(ctx context.Context, panelID string) ([]tb.CategorySlot, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.key,
		       CASE WHEN c.key = $1 THEN 1
		            ELSE COALESCE(NULLIF(split_part(c.key, ':', 2), '')::int, 0)
		       END AS slot,
		       c.category_id,
		       (SELECT COUNT(*) FROM discord_channels dc WHERE dc.category_id = c.category_id)
		FROM discord_server_config c
		WHERE c.active = TRUE
		  AND (c.key = $1 OR c.key LIKE $1 || ':%')
		ORDER BY slot`, panelID)
	if err != nil {
		return nil, fmt.Errorf("load category slots: %w", err)
	}
	defer rows.Close()

	var out []tb.CategorySlot
	for rows.Next() {
		var c tb.CategorySlot
		if err := rows.Scan(&c.Key, &c.Slot, &c.CategoryID, &c.Count); err != nil {
			return nil, fmt.Errorf("scan category slot: %w", err)
		}
		if c.Slot == 0 {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *DiscordStores) UpsertTranscript(ctx context.Context, ticketID int64, html string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ticket_transcripts (ticket_id, content) VALUES ($1, $2)
		ON CONFLICT (ticket_id) DO UPDATE SET content = EXCLUDED.content, generated_at = NOW()`, ticketID, html)
	return err
}

func (s *DiscordStores) UpsertGuild(ctx context.Context, guildID, name string, active bool) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO discord_guilds (guild_id, name, active) VALUES ($1, $2, $3)
		ON CONFLICT (guild_id) DO UPDATE SET name = EXCLUDED.name, active = EXCLUDED.active`, guildID, name, active)
	return err
}

func (s *DiscordStores) Insert(ctx context.Context, p *tb.TimedCloseParams) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO timed_closes (ticket_id, channel_id, actor, created_by, message_id, close_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (ticket_id) DO NOTHING`,
		p.TicketID, p.ChannelID, p.Actor, p.CreatedBy, p.MessageID, p.CloseAt)
	return tag.RowsAffected() > 0, err
}

func (s *DiscordStores) Delete(ctx context.Context, ticketID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM timed_closes WHERE ticket_id = $1`, ticketID)
	return tag.RowsAffected() > 0, err
}

func (s *DiscordStores) List(ctx context.Context) ([]tb.TimedCloseRecord, error) {
	return s.timedCloses(ctx, `SELECT ticket_id, channel_id, actor, created_by, message_id, close_at FROM timed_closes`)
}

func (s *DiscordStores) ListOverdue(ctx context.Context) ([]tb.TimedCloseRecord, error) {
	return s.timedCloses(ctx, `SELECT ticket_id, channel_id, actor, created_by, message_id, close_at FROM timed_closes WHERE close_at <= NOW()`)
}

func (s *DiscordStores) timedCloses(ctx context.Context, query string) ([]tb.TimedCloseRecord, error) {
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tb.TimedCloseRecord
	for rows.Next() {
		var r tb.TimedCloseRecord
		if err := rows.Scan(&r.TicketID, &r.ChannelID, &r.Actor, &r.CreatedBy, &r.MessageID, &r.CloseAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type scheduledCloseStore struct{ s *DiscordStores }

func (s *DiscordStores) ScheduledCloseStore() tb.ScheduledCloseStore {
	return &scheduledCloseStore{s: s}
}

func (sc *scheduledCloseStore) Insert(ctx context.Context, p tb.ScheduledCloseParams) (bool, error) {
	tag, err := sc.s.pool.Exec(ctx, `
		INSERT INTO scheduled_closes (ticket_id, channel_id, actor, close_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (ticket_id) DO NOTHING`, p.TicketID, p.ChannelID, p.Actor, p.CloseAt)
	return tag.RowsAffected() > 0, err
}

func (sc *scheduledCloseStore) Delete(ctx context.Context, ticketID int64) (bool, error) {
	tag, err := sc.s.pool.Exec(ctx, `DELETE FROM scheduled_closes WHERE ticket_id = $1`, ticketID)
	return tag.RowsAffected() > 0, err
}

func (sc *scheduledCloseStore) List(ctx context.Context) ([]tb.ScheduledCloseRecord, error) {
	return sc.query(ctx, `SELECT ticket_id, channel_id, actor, close_at FROM scheduled_closes`)
}

func (sc *scheduledCloseStore) ListOverdue(ctx context.Context) ([]tb.ScheduledCloseRecord, error) {
	return sc.query(ctx, `SELECT ticket_id, channel_id, actor, close_at FROM scheduled_closes WHERE close_at <= NOW()`)
}

func (sc *scheduledCloseStore) query(ctx context.Context, q string) ([]tb.ScheduledCloseRecord, error) {
	rows, err := sc.s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tb.ScheduledCloseRecord
	for rows.Next() {
		var r tb.ScheduledCloseRecord
		if err := rows.Scan(&r.TicketID, &r.ChannelID, &r.Actor, &r.CloseAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type buttonRepository struct{ s *DiscordStores }

func (s *DiscordStores) ButtonRepository() tb.ButtonRepository { return &buttonRepository{s: s} }

func (br *buttonRepository) GetButton(ctx context.Context, actionType string) (tb.ButtonConfig, error) {
	var c tb.ButtonConfig
	err := br.s.pool.QueryRow(ctx, `SELECT label, style, emoji FROM discord_buttons WHERE action_type = $1`, actionType).
		Scan(&c.Label, &c.Style, &c.Emoji)
	return c, err
}

type templateRepository struct{ s *DiscordStores }

func (s *DiscordStores) TemplateRepository() tb.TemplateRepository { return &templateRepository{s: s} }

func (tr *templateRepository) GetTemplateLayout(ctx context.Context, id string) (tb.TemplateLayout, error) {
	var l tb.TemplateLayout
	err := tr.s.pool.QueryRow(ctx, `SELECT layout_data, required_perm FROM discord_templates WHERE id = $1`, id).
		Scan(&l.LayoutData, &l.RequiredPerm)
	return l, err
}

func (tr *templateRepository) ListButtonEmojis(ctx context.Context, actionTypes []string) ([]tb.ButtonEmoji, error) {
	rows, err := tr.s.pool.Query(ctx, `SELECT action_type, emoji FROM discord_buttons WHERE action_type = ANY($1) AND emoji != ''`, actionTypes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tb.ButtonEmoji
	for rows.Next() {
		var e tb.ButtonEmoji
		if err := rows.Scan(&e.ActionType, &e.Emoji); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (tr *templateRepository) ListTemplates(ctx context.Context) ([]tb.TemplateInfo, error) {
	rows, err := tr.s.pool.Query(ctx, `SELECT id, name FROM discord_templates ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tb.TemplateInfo
	for rows.Next() {
		var t tb.TemplateInfo
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *DiscordStores) ListRoleCascade(ctx context.Context) ([]tb.RoleCascade, error) {
	rows, err := s.pool.Query(ctx, `SELECT role, implies_role FROM role_cascade ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tb.RoleCascade
	for rows.Next() {
		var c tb.RoleCascade
		if err := rows.Scan(&c.Role, &c.Implies); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *DiscordStores) ListRoleSyncConfig(ctx context.Context) ([]tb.RoleSyncMapping, error) {
	rows, err := s.pool.Query(ctx, `SELECT fga_role, guild_id, discord_role_id FROM role_sync_config ORDER BY fga_role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tb.RoleSyncMapping
	for rows.Next() {
		var m tb.RoleSyncMapping
		if err := rows.Scan(&m.Role, &m.GuildID, &m.DiscordRoleID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *DiscordStores) RecordSentMessage(ctx context.Context, messageID, channelID, guildID, templateID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sent_messages (message_id, channel_id, guild_id, template_id) VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (message_id) DO NOTHING`, messageID, channelID, guildID, templateID)
	return err
}

func (s *DiscordStores) SetButtonEmoji(ctx context.Context, actionType, emojiID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE discord_buttons SET emoji = $2 WHERE action_type = $1`, actionType, emojiID)
	return err
}

var _ tb.UnansweredStore = (*DiscordStores)(nil)

func (s *DiscordStores) ListUnanswered(ctx context.Context, after, repeat time.Duration) ([]tb.UnansweredTicket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.panel_id, t.created_by, dc.channel_id, t.wait_started_at, COALESCE(a.alerts, 0)
		FROM tickets t
		JOIN discord_channels dc ON dc.ticket_id = t.id
		LEFT JOIN ticket_unanswered_alerts a
		       ON a.ticket_id = t.id AND a.wait_started_at = t.wait_started_at
		WHERE t.status = 'open'
		  AND t.wait_started_at IS NOT NULL
		  AND t.wait_started_at <= NOW() - make_interval(secs => $1)
		  AND (a.ticket_id IS NULL
		       OR ($2 > 0 AND a.last_alert_at <= NOW() - make_interval(secs => $2)))
		ORDER BY t.wait_started_at`, after.Seconds(), repeat.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tb.UnansweredTicket
	for rows.Next() {
		var u tb.UnansweredTicket
		if err := rows.Scan(&u.TicketID, &u.PanelID, &u.CreatedBy, &u.ChannelID, &u.WaitStartedAt, &u.Alerts); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *DiscordStores) MarkUnansweredAlerted(ctx context.Context, ticketID int64, waitStartedAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ticket_unanswered_alerts (ticket_id, wait_started_at) VALUES ($1, $2)
		ON CONFLICT (ticket_id) DO UPDATE SET
		    alerts = CASE WHEN ticket_unanswered_alerts.wait_started_at = EXCLUDED.wait_started_at
		                  THEN ticket_unanswered_alerts.alerts + 1 ELSE 1 END,
		    wait_started_at = EXCLUDED.wait_started_at,
		    last_alert_at = NOW()`, ticketID, waitStartedAt)
	return err
}

func (s *DiscordStores) GuildLogChannel(ctx context.Context, guildID string) (string, error) {
	var ch string
	err := s.pool.QueryRow(ctx, `SELECT log_channel FROM discord_guilds WHERE guild_id = $1`, guildID).Scan(&ch)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return ch, err
}
