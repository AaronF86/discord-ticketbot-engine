package discord

import (
	"context"
	"log/slog"

	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

func (b *Bot) onMemberJoin(e *events.GuildMemberJoin) {
	go b.rejoinTicketChannels(context.Background(), e.Member.User.ID)
}

func (b *Bot) rejoinTicketChannels(ctx context.Context, userID snowflake.ID) {
	channelIDs, err := b.channelStore.ListActiveChannelIDsForUser(ctx, userID.String())
	if err != nil {
		slog.Error("rejoin: query active channels", "user_id", userID, "err", err)
		return
	}
	if len(channelIDs) == 0 {
		return
	}

	slog.Info("rejoin: restoring ticket channel access", "user_id", userID, "count", len(channelIDs))
	for _, chIDStr := range channelIDs {
		chID, err := snowflake.Parse(chIDStr)
		if err != nil {
			slog.Warn("rejoin: invalid channel_id", "channel_id", chIDStr, "err", err)
			continue
		}
		if err := b.AddUserToChannel(chID, userID); err != nil {
			slog.Warn("rejoin: add user to channel", "user_id", userID, "channel_id", chID, "err", err)
		}
	}
}
