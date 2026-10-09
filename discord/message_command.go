package discord

import (
	"context"
	"log/slog"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
)

func MessageCommand() discord.ApplicationCommandCreate {
	return discord.SlashCommandCreate{
		Name:        "message",
		Description: "Post a saved message template to this channel.",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:         "template",
				Description:  "The template to post",
				Required:     true,
				Autocomplete: true,
			},
		},
	}
}

func (b *Bot) RegisterMessageCommand(perms MessagePermChecker, sent SentMessageStore) {
	b.AddCommand(MessageCommand(), b.handleMessage(perms, sent))
	b.AddAutocompleteHandler("message", b.handleMessageAutocomplete)
}

func (b *Bot) handleMessage(perms MessagePermChecker, sent SentMessageStore) func(*events.ApplicationCommandInteractionCreate) {
	return func(e *events.ApplicationCommandInteractionCreate) {
		ctx := context.Background()
		templateID := e.SlashCommandInteractionData().String("template")
		sys := b.Msgs().System

		components, perm, err := b.Templates().CompileWithPanels(ctx, templateID, nil, b.Service().AllPanels())
		if err != nil {
			slog.Error("message command: compile failed", "template", templateID, "err", err)
			_ = e.CreateMessage(MsgEphemeral(sys.MessageNotFound))
			return
		}

		allowed, err := perms.CheckMessagePerm(ctx, e.User().ID.String(), perm)
		if err != nil {
			slog.Error("message command: permission check failed", "template", templateID, "perm", perm, "err", err)
			_ = e.CreateMessage(MsgEphemeral(sys.MessagePermFailed))
			return
		}
		if !allowed {
			_ = e.CreateMessage(MsgEphemeral(b.ForbiddenMsg("post that template")))
			return
		}

		msg, sendErr := e.Client().Rest.CreateMessage(e.Channel().ID(), discord.NewMessageCreateV2(components...))
		if sendErr != nil {
			slog.Error("message command: send failed", "template", templateID, "err", sendErr)
			_ = e.CreateMessage(MsgEphemeral(sys.MessageSendFailed))
			return
		}
		_ = e.CreateMessage(MsgEphemeral(sys.MessagePosted))

		if sent == nil {
			return
		}
		guildID := ""
		if e.GuildID() != nil {
			guildID = e.GuildID().String()
		}
		if err := sent.RecordSentMessage(ctx, msg.ID.String(), e.Channel().ID().String(), guildID, templateID); err != nil {
			slog.Warn("message command: failed to record sent message", "err", err)
		}
	}
}

func (b *Bot) handleMessageAutocomplete(e *events.AutocompleteInteractionCreate) {
	ctx := context.Background()

	opt, ok := e.Data.Options["template"]
	if !ok || !opt.Focused {
		_ = e.AutocompleteResult(nil)
		return
	}

	typed := strings.ToLower(opt.String())
	templates, err := b.Templates().List(ctx)
	if err != nil {
		slog.Error("message autocomplete: list failed", "err", err)
		_ = e.AutocompleteResult(nil)
		return
	}

	var choices []discord.AutocompleteChoice
	for _, t := range templates {
		if strings.Contains(strings.ToLower(t.Name), typed) || strings.Contains(strings.ToLower(t.ID), typed) {
			choices = append(choices, discord.AutocompleteChoiceString{Name: t.Name, Value: t.ID})
			if len(choices) == 25 {
				break
			}
		}
	}
	_ = e.AutocompleteResult(choices)
}
