package discord

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
	"git.aaronf86.tech/aaronf86/ticketbot/messages"
)

type AuditLogFunc func(ctx context.Context, actor, action string, ticketID int64, target string, meta map[string]any) error

type ButtonBuilderFn func(ctx context.Context, actionType, customID string) discord.ButtonComponent

type Bot struct {
	client     *bot.Client
	auth       core.Authorizer
	systemAuth core.SystemAuthorizer
	svc        *core.Service
	cfg        Config
	msgs       atomic.Pointer[messages.Messages]

	channelStore        ChannelStore
	guildStore          GuildStore
	timedCloseStore     TimedCloseStore
	scheduledCloseStore ScheduledCloseStore
	buttonRepo          ButtonRepository
	templateService     *TemplateService

	buildButton  ButtonBuilderFn
	systemEmojis map[string]string

	buttons              *ButtonRouter
	modals               *ModalRouter
	auditLog             AuditLogFunc
	cache                *Cache
	commands             []discord.ApplicationCommandCreate
	panelModals          map[string]func(snowflake.ID) discord.ModalCreate
	panelInterceptors    map[string]func(context.Context, *events.ComponentInteractionCreate) error
	panelCloseHandlers   map[string]func(*events.ApplicationCommandInteractionCreate, *core.Ticket)
	slashHandlers        map[string]func(*events.ApplicationCommandInteractionCreate)
	autocompleteHandlers map[string]func(*events.AutocompleteInteractionCreate)

	OnTicketCreated func(ctx context.Context, ticketID core.TicketID, actorID string) error

	scheduledClosesMu sync.Mutex
	scheduledCloses   map[core.TicketID]*time.Timer

	timedClosesMu sync.RWMutex
	timedCloses   map[core.TicketID]*timedCloseEntry
}

type Options struct {
	Token               string
	Service             *core.Service
	Config              Config
	ChannelStore        ChannelStore
	GuildStore          GuildStore
	TimedCloseStore     TimedCloseStore
	ScheduledCloseStore ScheduledCloseStore

	ButtonRepo   ButtonRepository
	TemplateRepo TemplateRepository

	Buttons *ButtonRouter
	Modals  *ModalRouter
}

func New(ctx context.Context, opts Options) (*Bot, error) {
	cache := NewCache()
	var tmplSvc *TemplateService
	if opts.TemplateRepo != nil {
		tmplSvc = NewTemplateService(opts.TemplateRepo, cache)
	}

	b := &Bot{
		auth:                 opts.Service.Auth(),
		svc:                  opts.Service,
		cfg:                  opts.Config,
		channelStore:         opts.ChannelStore,
		guildStore:           opts.GuildStore,
		timedCloseStore:      opts.TimedCloseStore,
		scheduledCloseStore:  opts.ScheduledCloseStore,
		buttonRepo:           opts.ButtonRepo,
		templateService:      tmplSvc,
		systemEmojis:         make(map[string]string),
		cache:                cache,
		panelModals:          make(map[string]func(snowflake.ID) discord.ModalCreate),
		panelInterceptors:    make(map[string]func(context.Context, *events.ComponentInteractionCreate) error),
		panelCloseHandlers:   make(map[string]func(*events.ApplicationCommandInteractionCreate, *core.Ticket)),
		slashHandlers:        make(map[string]func(*events.ApplicationCommandInteractionCreate)),
		autocompleteHandlers: make(map[string]func(*events.AutocompleteInteractionCreate)),
		scheduledCloses:      make(map[core.TicketID]*time.Timer),
		timedCloses:          make(map[core.TicketID]*timedCloseEntry),
	}

	if opts.Buttons != nil {
		b.buttons = opts.Buttons
	} else {
		b.buttons = NewButtonRouter(nil, nil)
	}
	if opts.Modals != nil {
		b.modals = opts.Modals
	} else {
		b.modals = NewModalRouter(nil, nil)
	}

	b.commands = buildCommands(opts.Service)

	client, err := disgo.New(opts.Token,
		bot.WithGatewayConfigOpts(gateway.WithIntents(
			gateway.IntentGuilds,
			gateway.IntentGuildMembers,
			gateway.IntentGuildMessages,
			gateway.IntentMessageContent,
		)),
		bot.WithEventListenerFunc(b.onSlashCommand),
		bot.WithEventListenerFunc(b.onAutocomplete),
		bot.WithEventListenerFunc(b.onComponentInteraction),
		bot.WithEventListenerFunc(b.onModalSubmit),
		bot.WithEventListenerFunc(b.onGuildMessage),
		bot.WithEventListenerFunc(b.onGuildMessageUpdate),
		bot.WithEventListenerFunc(b.onGuildReady),
		bot.WithEventListenerFunc(b.onGuildJoin),
		bot.WithEventListenerFunc(b.onGuildLeave),
	)
	if err != nil {
		return nil, err
	}
	b.client = client
	return b, nil
}

func (b *Bot) AddEventListener(l bot.EventListener) {
	b.client.AddEventListeners(l)
}

func (b *Bot) RegisterPanelModal(panelID string, fn func(msgID snowflake.ID) discord.ModalCreate) {
	b.panelModals[panelID] = fn
}

func (b *Bot) RegisterPanelInterceptor(panelID string, fn func(context.Context, *events.ComponentInteractionCreate) error) {
	b.panelInterceptors[panelID] = fn
}

func (b *Bot) RegisterPanelClose(panelID string, fn func(*events.ApplicationCommandInteractionCreate, *core.Ticket)) {
	b.panelCloseHandlers[panelID] = fn
}

func (b *Bot) AddCommand(cmd discord.ApplicationCommandCreate, handler func(*events.ApplicationCommandInteractionCreate)) {
	b.commands = append(b.commands, cmd)
	b.slashHandlers[cmd.(discord.SlashCommandCreate).Name] = handler
}

func (b *Bot) AddAutocompleteHandler(name string, fn func(*events.AutocompleteInteractionCreate)) {
	b.autocompleteHandlers[name] = fn
}

func (b *Bot) Service() *core.Service                { return b.svc }
func (b *Bot) Authorizer() core.Authorizer           { return b.auth }
func (b *Bot) SystemAuth() core.SystemAuthorizer     { return b.systemAuth }
func (b *Bot) SetSystemAuth(a core.SystemAuthorizer) { b.systemAuth = a }
func (b *Bot) BotConfig() Config                     { return b.cfg }
func (b *Bot) Buttons() *ButtonRouter                { return b.buttons }
func (b *Bot) Modals() *ModalRouter                  { return b.modals }
func (b *Bot) Cache() *Cache                         { return b.cache }
func (b *Bot) Rest() rest.Rest                       { return b.client.Rest }
func (b *Bot) ApplicationID() snowflake.ID           { return b.client.ApplicationID }

func (b *Bot) SetAuditLog(fn AuditLogFunc) { b.auditLog = fn }
func (b *Bot) AuditLog() AuditLogFunc      { return b.auditLog }

func (b *Bot) SetButtonBuilder(fn ButtonBuilderFn) { b.buildButton = fn }

func (b *Bot) Templates() *TemplateService { return b.templateService }

func (b *Bot) Msgs() *messages.Messages     { return b.msgs.Load() }
func (b *Bot) SetMsgs(m *messages.Messages) { b.msgs.Store(m) }

func (b *Bot) SystemEmoji(actionType string) string { return b.systemEmojis[actionType] }

func (b *Bot) SetSystemEmoji(actionType, emojiID string) {
	b.systemEmojis[actionType] = emojiID
}

func (b *Bot) CheckEmoji() string {
	if e := b.systemEmojis["check"]; e != "" {
		return e
	}
	return "✅"
}

func (b *Bot) XEmoji() string {
	if e := b.systemEmojis["x"]; e != "" {
		return e
	}
	return "❌"
}

func (b *Bot) ForbiddenMsg(action string) string {
	msgs := b.Msgs()
	if msgs == nil {
		return "Permission denied."
	}
	msg := messages.Format(msgs.Errors.Forbidden, "action", action)
	if e := b.systemEmojis["forbidden"]; e != "" {
		return e + " " + msg
	}
	return msg
}

type CreateTicketResult struct {
	TicketID  core.TicketID
	ChannelID snowflake.ID
}

func (b *Bot) CreateTicket(ctx context.Context, guildID snowflake.ID, panelID, actorID string) (CreateTicketResult, error) {
	actorSnow, err := snowflake.Parse(actorID)
	if err != nil {
		return CreateTicketResult{}, fmt.Errorf("invalid actor ID: %w", err)
	}
	ticket, err := b.svc.Open(ctx, actorID, panelID)
	if err != nil {
		return CreateTicketResult{}, err
	}
	if b.OnTicketCreated != nil {
		if err := b.OnTicketCreated(ctx, ticket.ID, actorID); err != nil {
			return CreateTicketResult{}, err
		}
	}
	channelID, err := b.openChannel(ctx, guildID, panelID, ticket, actorSnow)
	if err != nil {
		return CreateTicketResult{}, err
	}
	return CreateTicketResult{TicketID: ticket.ID, ChannelID: channelID}, nil
}

func (b *Bot) Post(channelID snowflake.ID, msg discord.MessageCreate) error {
	_, err := b.client.Rest.CreateMessage(channelID, msg)
	return err
}

func (b *Bot) PostMessage(channelID snowflake.ID, msg discord.MessageCreate) (discord.Message, error) {
	m, err := b.client.Rest.CreateMessage(channelID, msg)
	if err != nil {
		return discord.Message{}, err
	}
	return *m, nil
}

func (b *Bot) ArchiveAndClose(channelID snowflake.ID, ticket *core.Ticket, panelName string) {
	b.archiveAndClose(channelID, ticket, panelName)
}

func (b *Bot) TicketFromChannel(ctx context.Context, channelID snowflake.ID) (core.TicketID, error) {
	return b.ticketFromChannel(ctx, channelID)
}

func (b *Bot) Start() error {
	return b.client.OpenGateway(context.Background())
}

func (b *Bot) Stop() {
	b.client.Close(context.Background())
}

func (b *Bot) onComponentInteraction(e *events.ComponentInteractionCreate) {
	b.buttons.dispatch(context.Background(), e)
}

func (b *Bot) onModalSubmit(e *events.ModalSubmitInteractionCreate) {
	b.modals.dispatch(context.Background(), e)
}

func (b *Bot) onGuildReady(e *events.GuildReady) {
	b.upsertGuild(e.GuildID.String(), e.Guild.Name, true)
	b.registerCommands(e.GuildID)
	slog.Info("guild ready", "guild_id", e.GuildID.String(), "guild_name", e.Guild.Name)
}

func (b *Bot) onGuildJoin(e *events.GuildJoin) {
	b.upsertGuild(e.GuildID.String(), e.Guild.Name, true)
	b.registerCommands(e.GuildID)
	slog.Info("guild joined", "guild_id", e.GuildID.String(), "guild_name", e.Guild.Name)
}

func (b *Bot) onGuildLeave(e *events.GuildLeave) {
	b.upsertGuild(e.GuildID.String(), e.Guild.Name, false)
	slog.Info("guild left", "guild_id", e.GuildID.String())
}

func (b *Bot) registerCommands(guildID snowflake.ID) {
	if _, err := b.client.Rest.SetGuildCommands(b.client.ApplicationID, guildID, b.commands); err != nil {
		slog.Error("failed to register commands", "guild_id", guildID, "err", err)
	}
}

func (b *Bot) upsertGuild(guildID, name string, active bool) {
	if b.guildStore == nil {
		return
	}
	ctx := context.Background()
	if err := b.guildStore.UpsertGuild(ctx, guildID, name, active); err != nil {
		slog.Error("upsert guild failed", "guild_id", guildID, "err", err)
	}
}

func (b *Bot) onGuildMessage(e *events.GuildMessageCreate) {
	if e.Message.Author.Bot {
		return
	}
	ctx := context.Background()
	ticketID, err := b.ticketFromChannel(ctx, e.ChannelID)
	if err != nil {
		return
	}
	b.MaybeCancelTimedClose(ctx, ticketID, e.Message.Author.ID.String())
	if err := b.svc.RecordMessage(ctx, ticketID, e.Message.Author.ID.String()); err != nil {
		slog.Error("failed to record message", "ticket", ticketID, "err", err)
	}
}

func (b *Bot) onGuildMessageUpdate(e *events.GuildMessageUpdate) {
	if b.auditLog == nil {
		return
	}
	if e.Message.Author.Bot {
		return
	}
	if e.Message.Content == e.OldMessage.Content {
		return
	}
	ctx := context.Background()
	ticketID, err := b.ticketFromChannel(ctx, e.ChannelID)
	if err != nil {
		return
	}
	actor := e.Message.Author.ID.String()
	meta := map[string]any{
		"message_id": e.MessageID.String(),
		"before":     e.OldMessage.Content,
		"after":      e.Message.Content,
	}
	if err := b.auditLog(ctx, actor, "message.edited", int64(ticketID), e.MessageID.String(), meta); err != nil {
		slog.Error("audit log: message edit", "ticket", ticketID, "err", err)
	}
}

func (b *Bot) onAutocomplete(e *events.AutocompleteInteractionCreate) {
	name := e.Data.CommandName
	if h, ok := b.autocompleteHandlers[name]; ok {
		h(e)
	}
}

func (b *Bot) onSlashCommand(e *events.ApplicationCommandInteractionCreate) {
	name := e.SlashCommandInteractionData().CommandName()
	guildStr := ""
	if e.GuildID() != nil {
		guildStr = e.GuildID().String()
	}
	slog.Debug("command", "name", name, "user", e.User().ID.String(), "guild", guildStr)
	switch name {
	case "open":
		b.handleOpen(e)
	case "close":
		b.handleClose(e)
	case "force-close":
		b.handleForceClose(e)
	case "timed-close":
		b.handleTimedClose(e)
	case "claim":
		b.handleClaim(e)
	case "unclaim":
		b.handleUnclaim(e)
	case "add-user":
		b.handleAddUser(e)
	case "remove-user":
		b.handleRemoveUser(e)
	case "set-status":
		b.handleSetStatus(e)
	default:
		if h, ok := b.slashHandlers[name]; ok {
			h(e)
		}
	}
}

func buildCommands(svc *core.Service) []discord.ApplicationCommandCreate {
	panelChoices := make([]discord.ApplicationCommandOptionChoiceString, 0)
	for _, p := range svc.AllPanels() {
		panelChoices = append(panelChoices, discord.ApplicationCommandOptionChoiceString{
			Name:  p.Name,
			Value: p.ID,
		})
	}

	return []discord.ApplicationCommandCreate{
		discord.SlashCommandCreate{
			Name:        "open",
			Description: "Open a support ticket",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{
					Name:        "panel",
					Description: "Which type of support do you need?",
					Required:    true,
					Choices:     panelChoices,
				},
			},
		},
		discord.SlashCommandCreate{Name: "close", Description: "Close this ticket"},
		discord.SlashCommandCreate{
			Name:        "force-close",
			Description: "Force close this ticket regardless of status",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{
					Name:        "delay",
					Description: "Schedule close after this duration, e.g. 10h, 30m, 1h 30m",
					Required:    false,
				},
			},
		},
		discord.SlashCommandCreate{
			Name:        "timed-close",
			Description: "Close this ticket after a delay unless the client responds (default: 24h)",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{
					Name:        "delay",
					Description: "How long to wait before closing, e.g. 10h, 30m, 1h 30m (default: 24h)",
					Required:    false,
				},
			},
		},
		discord.SlashCommandCreate{Name: "claim", Description: "Claim this ticket"},
		discord.SlashCommandCreate{Name: "unclaim", Description: "Unclaim this ticket"},
		discord.SlashCommandCreate{
			Name:        "add-user",
			Description: "Add a user to this ticket",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionUser{
					Name:        "user",
					Description: "The user to add",
					Required:    true,
				},
			},
		},
		discord.SlashCommandCreate{
			Name:        "remove-user",
			Description: "Remove a user from this ticket",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionUser{
					Name:        "user",
					Description: "The user to remove",
					Required:    true,
				},
			},
		},
		discord.SlashCommandCreate{
			Name:        "set-status",
			Description: "Set the status of this ticket",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{
					Name:        "status",
					Description: "The new status",
					Required:    true,
					Choices: []discord.ApplicationCommandOptionChoiceString{
						{Name: "Open", Value: "open"},
					},
				},
			},
		},
	}
}

func replyComponentErr(e *events.ComponentInteractionCreate) {
	_ = e.CreateMessage(MsgEphemeral("Something went wrong. Please try again."))
}

func MsgEphemeral(content string) discord.MessageCreate {
	return discord.MessageCreate{Content: content, Flags: discord.MessageFlagEphemeral}
}

func (b *Bot) ticketAndPanel(ctx context.Context, id core.TicketID) (*core.Ticket, string) {
	t, err := b.svc.Get(ctx, id)
	if err != nil {
		return &core.Ticket{ID: id}, "Support"
	}
	if p, ok := b.svc.Panel(t.PanelID); ok {
		return t, p.Name
	}
	return t, t.PanelID
}

func (b *Bot) dmTranscript(panelID string) bool {
	p, ok := b.svc.Panel(panelID)
	return !ok || p.DMTranscript
}

func (b *Bot) closeSuccessMsg(panelID string) string {
	msgs := b.Msgs()
	if msgs == nil {
		return "Ticket closed."
	}
	if b.dmTranscript(panelID) {
		return msgs.Ticket.CloseSuccess
	}
	return msgs.Ticket.CloseSuccessNoTranscript
}

func (b *Bot) forceCloseSuccessMsg(panelID string) string {
	msgs := b.Msgs()
	if msgs == nil {
		return "Ticket force-closed."
	}
	if b.dmTranscript(panelID) {
		return msgs.Ticket.ForceCloseSuccess
	}
	return msgs.Ticket.ForceCloseSuccessNoTranscript
}
