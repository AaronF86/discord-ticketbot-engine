package discord

import (
	"context"
	"log/slog"

	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"golang.org/x/sync/errgroup"
)

type RoleSyncService struct {
	roles RoleSource
	store RoleSyncStore
	rest  rest.Rest
}

func NewRoleSyncService(roles RoleSource, store RoleSyncStore, r rest.Rest) *RoleSyncService {
	return &RoleSyncService{roles: roles, store: store, rest: r}
}

func (s *RoleSyncService) SyncUser(ctx context.Context, userID string) {
	held, err := s.roles.ListUserRoles(ctx, userID)
	if err != nil {
		slog.Error("role sync: list roles", "user", userID, "err", err)
		return
	}

	cascade, err := s.store.ListRoleCascade(ctx)
	if err != nil {
		slog.Error("role sync: load cascade", "err", err)
		return
	}
	config, err := s.store.ListRoleSyncConfig(ctx)
	if err != nil {
		slog.Error("role sync: load config", "err", err)
		return
	}

	byGuild := PlanRoleSync(held, cascade, config)

	userSnow, err := snowflake.Parse(userID)
	if err != nil {
		slog.Error("role sync: parse user snowflake", "user", userID, "err", err)
		return
	}

	g, gctx := errgroup.WithContext(ctx)
	for guildIDStr, entries := range byGuild {
		guildSnow, err := snowflake.Parse(guildIDStr)
		if err != nil {
			slog.Warn("role sync: skip invalid guild id", "guild", guildIDStr)
			continue
		}
		g.Go(func() error {
			s.syncGuild(gctx, guildSnow, userSnow, entries)
			return nil
		})
	}
	_ = g.Wait()
}

type RoleSyncEntry struct {
	DiscordRoleID string
	ShouldHave    bool
}

func PlanRoleSync(held []string, cascade []RoleCascade, config []RoleSyncMapping) map[string][]RoleSyncEntry {
	implied := make(map[string][]string, len(cascade))
	for _, c := range cascade {
		implied[c.Role] = append(implied[c.Role], c.Implies)
	}

	has := make(map[string]bool, len(held))
	for _, r := range held {
		has[r] = true
		for _, i := range implied[r] {
			has[i] = true
		}
	}

	type key struct{ guild, role string }
	want := make(map[key]bool)
	for _, m := range config {
		k := key{m.GuildID, m.DiscordRoleID}
		want[k] = want[k] || has[m.Role]
	}

	out := make(map[string][]RoleSyncEntry)
	for k, w := range want {
		out[k.guild] = append(out[k.guild], RoleSyncEntry{DiscordRoleID: k.role, ShouldHave: w})
	}
	return out
}

func (s *RoleSyncService) syncGuild(_ context.Context, guildID, userID snowflake.ID, entries []RoleSyncEntry) {
	member, err := s.rest.GetMember(guildID, userID)
	if err != nil {
		slog.Debug("role sync: member not in guild", "guild", guildID, "user", userID, "err", err)
		return
	}

	current := make(map[snowflake.ID]bool, len(member.RoleIDs))
	for _, rid := range member.RoleIDs {
		current[rid] = true
	}

	for _, e := range entries {
		roleSnow, err := snowflake.Parse(e.DiscordRoleID)
		if err != nil {
			slog.Warn("role sync: skip invalid role id", "role", e.DiscordRoleID)
			continue
		}
		if e.ShouldHave == current[roleSnow] {
			continue
		}
		s.applyRole(guildID, userID, roleSnow, e.ShouldHave)
	}
}

func (s *RoleSyncService) applyRole(guildID, userID, roleID snowflake.ID, add bool) {
	verb, change := "add", s.rest.AddMemberRole
	past := "added"
	if !add {
		verb, change, past = "remove", s.rest.RemoveMemberRole, "removed"
	}
	if err := change(guildID, userID, roleID); err != nil {
		slog.Error("role sync: "+verb+" role", "guild", guildID, "user", userID, "role", roleID, "err", err)
		return
	}
	slog.Info("role sync: "+past+" role", "guild", guildID, "user", userID, "role", roleID)
}
