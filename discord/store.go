package discord

import (
	"context"
	"time"
)

type ChannelStore interface {
	SetChannelTicket(ctx context.Context, channelID string, ticketID int64, categoryID string) error
	GetTicketByChannel(ctx context.Context, channelID string) (int64, error)
	DeleteChannel(ctx context.Context, channelID string) error

	ListActiveChannelIDsForUser(ctx context.Context, userID string) ([]string, error)

	GetServerConfig(ctx context.Context, key string) (categoryID string, err error)

	InsertCategoryIfNotExists(ctx context.Context, key, guildID, categoryID string) error

	LoadCategorySlots(ctx context.Context, panelID string) ([]CategorySlot, error)

	UpsertTranscript(ctx context.Context, ticketID int64, html string) error
}

type CategorySlot struct {
	Key        string
	Slot       int
	CategoryID string
	Count      int
}

type GuildStore interface {
	UpsertGuild(ctx context.Context, guildID, name string, active bool) error
}

type TimedCloseStore interface {
	Insert(ctx context.Context, p *TimedCloseParams) (inserted bool, err error)
	Delete(ctx context.Context, ticketID int64) (deleted bool, err error)
	List(ctx context.Context) ([]TimedCloseRecord, error)
	ListOverdue(ctx context.Context) ([]TimedCloseRecord, error)
}

type TimedCloseParams struct {
	TicketID  int64
	ChannelID string
	Actor     string
	CreatedBy string
	MessageID string
	CloseAt   time.Time
}

type TimedCloseRecord struct {
	TicketID  int64
	ChannelID string
	Actor     string
	CreatedBy string
	MessageID string
	CloseAt   time.Time
}

type ScheduledCloseStore interface {
	Insert(ctx context.Context, p ScheduledCloseParams) (inserted bool, err error)
	Delete(ctx context.Context, ticketID int64) (deleted bool, err error)
	List(ctx context.Context) ([]ScheduledCloseRecord, error)
	ListOverdue(ctx context.Context) ([]ScheduledCloseRecord, error)
}

type ScheduledCloseParams struct {
	TicketID  int64
	ChannelID string
	Actor     string
	CloseAt   time.Time
}

type ScheduledCloseRecord struct {
	TicketID  int64
	ChannelID string
	Actor     string
	CloseAt   time.Time
}

type ButtonConfig struct {
	Label string
	Style int32
	Emoji string
}

type TemplateLayout struct {
	LayoutData   []byte
	RequiredPerm string
}

type ButtonEmoji struct {
	ActionType string
	Emoji      string
}

type TemplateInfo struct {
	ID   string
	Name string
}

type TemplateRepository interface {
	GetTemplateLayout(ctx context.Context, id string) (TemplateLayout, error)
	ListButtonEmojis(ctx context.Context, actionTypes []string) ([]ButtonEmoji, error)
	ListTemplates(ctx context.Context) ([]TemplateInfo, error)
}

type RoleSource interface {
	ListUserRoles(ctx context.Context, userID string) ([]string, error)
}

type RoleCascade struct {
	Role    string
	Implies string
}

type RoleSyncMapping struct {
	Role          string
	GuildID       string
	DiscordRoleID string
}

type RoleSyncStore interface {
	ListRoleCascade(ctx context.Context) ([]RoleCascade, error)
	ListRoleSyncConfig(ctx context.Context) ([]RoleSyncMapping, error)
}

type SentMessageStore interface {
	RecordSentMessage(ctx context.Context, messageID, channelID, guildID, templateID string) error
}

type EmojiStore interface {
	SetButtonEmoji(ctx context.Context, actionType, emojiID string) error
}

type UnansweredTicket struct {
	TicketID      int64
	PanelID       string
	CreatedBy     string
	ChannelID     string
	WaitStartedAt time.Time
	Alerts        int
}

type UnansweredStore interface {
	ListUnanswered(ctx context.Context, after, repeat time.Duration) ([]UnansweredTicket, error)
	MarkUnansweredAlerted(ctx context.Context, ticketID int64, waitStartedAt time.Time) error
	GuildLogChannel(ctx context.Context, guildID string) (string, error)
}
