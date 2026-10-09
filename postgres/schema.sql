CREATE TABLE IF NOT EXISTS tickets (
    id                       BIGSERIAL   PRIMARY KEY,
    panel_id                 TEXT        NOT NULL,
    status                   TEXT        NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_message             TIMESTAMPTZ,
    left_on_read             BOOLEAN     NOT NULL DEFAULT FALSE,
    created_by               TEXT        NOT NULL,
    claimed_by               TEXT        NOT NULL DEFAULT '',
    close_requested_by       TEXT        NOT NULL DEFAULT '',
    workflow_completed_steps TEXT[]      NOT NULL DEFAULT '{}',
    first_staff_reply_at     TIMESTAMPTZ,
    wait_started_at          TIMESTAMPTZ,
    total_wait_secs          DOUBLE PRECISION NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS ticket_users (
    ticket_id BIGINT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    user_id   TEXT NOT NULL,
    PRIMARY KEY (ticket_id, user_id)
);

CREATE TABLE IF NOT EXISTS ticket_metrics (
    ticket_id BIGINT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    key       TEXT NOT NULL,
    kind      TEXT NOT NULL,
    value     TEXT NOT NULL,
    PRIMARY KEY (ticket_id, key)
);

CREATE TABLE IF NOT EXISTS ticket_meta (
    ticket_id BIGINT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    key       TEXT   NOT NULL,
    value     TEXT   NOT NULL,
    PRIMARY KEY (ticket_id, key)
);

CREATE TABLE IF NOT EXISTS ticket_transcripts (
    ticket_id    BIGINT      PRIMARY KEY REFERENCES tickets(id) ON DELETE CASCADE,
    content      TEXT        NOT NULL,
    generated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS panel_settings (
    panel_id    TEXT PRIMARY KEY,
    max_tickets INT  NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS discord_channels (
    channel_id  TEXT   PRIMARY KEY,
    ticket_id   BIGINT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    category_id TEXT   NOT NULL DEFAULT ''
);
ALTER TABLE discord_channels ADD COLUMN IF NOT EXISTS category_id TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS discord_guilds (
    guild_id    TEXT        PRIMARY KEY,
    name        TEXT        NOT NULL,
    log_channel TEXT        NOT NULL DEFAULT '',
    active      BOOLEAN     NOT NULL DEFAULT TRUE,
    joined_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS discord_server_config (
    key         TEXT    PRIMARY KEY,
    guild_id    TEXT    NOT NULL,
    category_id TEXT    NOT NULL,
    active      BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE IF NOT EXISTS discord_templates (
    id          TEXT PRIMARY KEY,
    title       TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    color       INT  NOT NULL DEFAULT 5814783,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE discord_templates ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT '';
ALTER TABLE discord_templates ADD COLUMN IF NOT EXISTS layout_data JSONB;
ALTER TABLE discord_templates ADD COLUMN IF NOT EXISTS required_perm TEXT NOT NULL DEFAULT 'management_message';

CREATE TABLE IF NOT EXISTS sent_messages (
    message_id  TEXT        PRIMARY KEY,
    channel_id  TEXT        NOT NULL,
    guild_id    TEXT        NOT NULL,
    template_id TEXT        REFERENCES discord_templates(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS discord_buttons (
    action_type TEXT PRIMARY KEY,
    label       TEXT NOT NULL,
    style       INT  NOT NULL DEFAULT 1,
    emoji       TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS role_sync_config (
    fga_role        TEXT NOT NULL,
    guild_id        TEXT NOT NULL,
    discord_role_id TEXT NOT NULL,
    PRIMARY KEY (fga_role, guild_id, discord_role_id)
);

CREATE TABLE IF NOT EXISTS role_cascade (
    role         TEXT NOT NULL,
    implies_role TEXT NOT NULL,
    PRIMARY KEY (role, implies_role)
);

CREATE TABLE IF NOT EXISTS timed_closes (
    ticket_id  BIGINT      PRIMARY KEY,
    channel_id TEXT        NOT NULL,
    actor      TEXT        NOT NULL,
    created_by TEXT        NOT NULL,
    message_id TEXT        NOT NULL,
    close_at   TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS scheduled_closes (
    ticket_id  BIGINT      PRIMARY KEY,
    channel_id TEXT        NOT NULL,
    actor      TEXT        NOT NULL,
    close_at   TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_logs (
    id         BIGSERIAL   PRIMARY KEY,
    actor      TEXT        NOT NULL,
    action     TEXT        NOT NULL,
    ticket_id  BIGINT,
    target     TEXT        NOT NULL DEFAULT '',
    metadata   JSONB       NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS audit_logs_ticket_id  ON audit_logs (ticket_id) WHERE ticket_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS audit_logs_actor      ON audit_logs (actor);
CREATE INDEX IF NOT EXISTS audit_logs_created_at ON audit_logs (created_at DESC);

CREATE TABLE IF NOT EXISTS audit_broadcast_queue (
    id            BIGSERIAL   PRIMARY KEY,
    audit_log_id  BIGINT      NOT NULL REFERENCES audit_logs(id),
    content       TEXT        NOT NULL,
    attempts      INT         NOT NULL DEFAULT 0,
    last_error    TEXT        NOT NULL DEFAULT '',
    next_retry_at TIMESTAMPTZ,
    sent_at       TIMESTAMPTZ
);

UPDATE discord_buttons SET action_type = 'ticket:close_req'
WHERE action_type = 'gs:close_req'
  AND NOT EXISTS (SELECT 1 FROM discord_buttons WHERE action_type = 'ticket:close_req');
DELETE FROM discord_buttons WHERE action_type = 'gs:close_req';

CREATE TABLE IF NOT EXISTS ticket_unanswered_alerts (
    ticket_id       BIGINT      PRIMARY KEY REFERENCES tickets(id) ON DELETE CASCADE,
    wait_started_at TIMESTAMPTZ NOT NULL,
    last_alert_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    alerts          INT         NOT NULL DEFAULT 1
);

ALTER TABLE tickets ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE ticket_users ADD COLUMN IF NOT EXISTS added_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
