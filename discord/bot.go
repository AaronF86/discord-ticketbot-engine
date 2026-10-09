package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
	emojiMu      sync.RWMutex

	buttons              *ButtonRouter
	modals               *ModalRouter
	auditLog             AuditLogFunc
	cache                *Cache
	extraCommands        []discord.ApplicationCommandCreate
	commandsMu           sync.RWMutex
	guilds               map[snowflake.ID]struct{}
	started              bool
	closing              sync.WaitGroup
	onTicketDiscarded    func(ctx context.Context, ticketID core.TicketID, actorID string)
	shutdownTimeout      time.Duration
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

	Messages      *messages.Messages
	SystemAuth    core.SystemAuthorizer
	AuditLog      AuditLogFunc
	ButtonBuilder ButtonBuilderFn

	OnTicketCreated   func(ctx context.Context, ticketID core.TicketID, actorID string) error
	OnTicketDiscarded func(ctx context.Context, ticketID core.TicketID, actorID string)

	ShutdownTimeout time.Duration
}

func (o *Options) Validate() error {
	var errs []error
	if o.Token == "" {
		errs = append(errs, errors.New("Options.Token is required"))
	}
	if o.Service == nil {
		errs = append(errs, errors.New("Service is required"))
	}
	if o.ChannelStore == nil {
		errs = append(errs, errors.New("ChannelStore is required"))
	}
	if (o.TimedCloseStore == nil) != (o.ScheduledCloseStore == nil) {
		errs = append(errs, errors.New("TimedCloseStore and ScheduledCloseStore must be set together or not at all"))
	}
	if o.TemplateRepo != nil && o.ButtonRepo == nil {
		errs = append(errs, errors.New("TemplateRepo needs ButtonRepo to render template buttons"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("discord.Options: %w", err)
	}
	return nil
}

func New(ctx context.Context, opts *Options) (*Bot, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if opts.TimedCloseStore == nil {
		slog.Warn("discord.New: no TimedCloseStore/ScheduledCloseStore; delayed closes are kept in memory and lost on restart")
	}
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
		guilds:               make(map[snowflake.ID]struct{}),
		panelModals:          make(map[string]func(snowflake.ID) discord.ModalCreate),
		panelInterceptors:    make(map[string]func(context.Context, *events.ComponentInteractionCreate) error),
		panelCloseHandlers:   make(map[string]func(*events.ApplicationCommandInteractionCreate, *core.Ticket)),
		slashHandlers:        make(map[string]func(*events.ApplicationCommandInteractionCreate)),
		autocompleteHandlers: make(map[string]func(*events.AutocompleteInteractionCreate)),
		scheduledCloses:      make(map[core.TicketID]*time.Timer),
		timedCloses:          make(map[core.TicketID]*timedCloseEntry),
		systemAuth:           opts.SystemAuth,
		auditLog:             opts.AuditLog,
		buildButton:          opts.ButtonBuilder,
		OnTicketCreated:      opts.OnTicketCreated,
		onTicketDiscarded:    opts.OnTicketDiscarded,
		shutdownTimeout:      opts.ShutdownTimeout,
	}
	if opts.Messages != nil {
		b.msgs.Store(opts.Messages)
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
	if b.buttons.getError == nil {
		b.buttons.getError = func() string { return b.Msgs().System.ModalError }
	}
	if b.modals.getModalError == nil {
		b.modals.SetModalError(func() string { return b.Msgs().System.ModalError })
	}

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
		bot.WithEventListenerFunc(b.onMemberJoin),
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
	b.commandsMu.Lock()
	b.extraCommands = slices.DeleteFunc(b.extraCommands, func(c discord.ApplicationCommandCreate) bool {
		return c.CommandName() == cmd.CommandName()
	})
	b.extraCommands = append(b.extraCommands, cmd)
	b.slashHandlers[cmd.CommandName()] = handler
	var live []snowflake.ID
	if b.started {
		for g := range b.guilds {
			live = append(live, g)
		}
	}
	b.commandsMu.Unlock()
	for _, g := range live {
		b.registerCommands(g)
	}
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

func (b *Bot) Msgs() *messages.Messages {
	if m := b.msgs.Load(); m != nil {
		return m
	}
	return defaultMsgs
}

var defaultMsgs = messages.Default()

func (b *Bot) SetMsgs(m *messages.Messages) { b.msgs.Store(m) }

func (b *Bot) SystemEmoji(actionType string) string {
	b.emojiMu.RLock()
	defer b.emojiMu.RUnlock()
	return b.systemEmojis[actionType]
}

func (b *Bot) SetSystemEmoji(actionType, emojiID string) {
	b.emojiMu.Lock()
	b.systemEmojis[actionType] = emojiID
	b.emojiMu.Unlock()
}

func (b *Bot) CheckEmoji() string {
	if e := b.SystemEmoji("check"); e != "" {
		return e
	}
	return "✅"
}

func (b *Bot) XEmoji() string {
	if e := b.SystemEmoji("x"); e != "" {
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
	if e := b.SystemEmoji("forbidden"); e != "" {
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
	var (
		channelID  snowflake.ID
		registered core.TicketID
	)
	ticket, err := b.svc.Open(ctx, actorID, panelID, core.WithProvision(func(ctx context.Context, t *core.Ticket) error {
		if b.OnTicketCreated != nil {
			if err := b.OnTicketCreated(ctx, t.ID, actorID); err != nil {
				return err
			}
			registered = t.ID
		}
		id, err := b.openChannel(ctx, guildID, panelID, t, actorSnow)
		channelID = id
		return err
	}))
	if err != nil {
		if channelID != 0 {
			b.removeChannel(context.WithoutCancel(ctx), channelID)
		}
		if registered != 0 && b.onTicketDiscarded != nil {
			b.onTicketDiscarded(context.WithoutCancel(ctx), registered, actorID)
		}
		return CreateTicketResult{}, err
	}
	return CreateTicketResult{TicketID: ticket.ID, ChannelID: channelID}, nil
}

func (b *Bot) Post(channelID snowflake.ID, msg *discord.MessageCreate) error {
	_, err := b.client.Rest.CreateMessage(channelID, *msg)
	return err
}

func (b *Bot) PostMessage(channelID snowflake.ID, msg *discord.MessageCreate) (discord.Message, error) {
	m, err := b.client.Rest.CreateMessage(channelID, *msg)
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
	b.commandsMu.Lock()
	b.started = true
	b.commandsMu.Unlock()
	return b.client.OpenGateway(context.Background())
}

func (b *Bot) Stop() {
	b.client.Close(context.Background())

	timeout := b.shutdownTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	done := make(chan struct{})
	go func() { b.closing.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("stop: gave up waiting for ticket teardowns", "timeout", timeout)
	}
}

func (b *Bot) onComponentInteraction(e *events.ComponentInteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), interactionTimeout)
	defer cancel()
	b.buttons.dispatch(ctx, e)
}

func (b *Bot) onModalSubmit(e *events.ModalSubmitInteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), interactionTimeout)
	defer cancel()
	b.modals.dispatch(ctx, e)
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
	b.commandsMu.Lock()
	b.guilds[guildID] = struct{}{}
	cmds := b.buildAllCommands()
	b.commandsMu.Unlock()
	if _, err := b.client.Rest.SetGuildCommands(b.client.ApplicationID, guildID, cmds); err != nil {
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

//nolint:cyclop // dispatch switch
func (b *Bot) onSlashCommand(e *events.ApplicationCommandInteractionCreate) {
	name := e.SlashCommandInteractionData().CommandName()
	guildStr := ""
	if e.GuildID() != nil {
		guildStr = e.GuildID().String()
	}
	slog.Debug("command", "name", name, "user", e.User().ID.String(), "guild", guildStr)
	b.commandsMu.RLock()
	custom, hasCustom := b.slashHandlers[name]
	b.commandsMu.RUnlock()
	if hasCustom {
		custom(e)
		return
	}
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
	}
}

func (b *Bot) buildAllCommands() []discord.ApplicationCommandCreate {
	builtin := buildCommands(b.svc)
	out := make([]discord.ApplicationCommandCreate, 0, len(builtin)+len(b.extraCommands))
	for _, c := range builtin {
		if _, overridden := b.slashHandlers[c.CommandName()]; overridden {
			continue
		}
		out = append(out, c)
	}
	return append(out, b.extraCommands...)
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

func replyComponentErr(e *events.ComponentInteractionCreate, msg string) {
	_ = e.CreateMessage(MsgEphemeral(msg))
}

func MsgEphemeral(content string) discord.MessageCreate {
	return discord.MessageCreate{Content: content, Flags: discord.MessageFlagEphemeral}
}

func (b *Bot) ticketAndPanel(ctx context.Context, id core.TicketID) (ticket *core.Ticket, panelName string) {
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
