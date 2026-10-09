package discord

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"

	"github.com/disgoorg/disgo/discord"
	"golang.org/x/sync/errgroup"
)

type EmojiDef struct {
	ActionType string
	Name       string
	Image      []byte
}

type EmojiSpec struct {
	ActionType string
	Name       string
}

const IconTypeAVIF discord.IconType = "image/avif"

func EmojiDefsFromFS(fsys fs.FS, dir string, specs []EmojiSpec) []EmojiDef {
	out := make([]EmojiDef, 0, len(specs))
	for _, s := range specs {
		data, err := fs.ReadFile(fsys, dir+"/"+s.Name+".avif")
		if err != nil {
			slog.Warn("sync emojis: file not found, skipping", "name", s.Name)
			continue
		}
		out = append(out, EmojiDef{ActionType: s.ActionType, Name: s.Name, Image: data})
	}
	return out
}

func (b *Bot) SyncEmojis(ctx context.Context, store EmojiStore, defs []EmojiDef) error {
	existing, err := b.Rest().GetApplicationEmojis(b.ApplicationID())
	if err != nil {
		return fmt.Errorf("sync emojis: list existing: %w", err)
	}
	byName := make(map[string]discord.Emoji, len(existing))
	for _, e := range existing {
		byName[e.Name] = e
	}

	type synced struct {
		actionType string
		name       string
		emoji      discord.Emoji
	}
	var (
		mu      sync.Mutex
		results []synced
	)

	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, 3)

	for _, def := range defs {
		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			emoji, err := b.ensureEmoji(byName, def)
			if err != nil {
				return err
			}
			if err := store.SetButtonEmoji(gctx, def.ActionType, emoji.ID.String()); err != nil {
				return fmt.Errorf("sync emojis: store id for %s: %w", def.ActionType, err)
			}

			mu.Lock()
			results = append(results, synced{def.ActionType, def.Name, emoji})
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	for _, r := range results {
		b.SetSystemEmoji(r.actionType, fmt.Sprintf("<:%s:%s>", r.name, r.emoji.ID))
		if panelID, isPanel := strings.CutPrefix(r.actionType, "panel:"); isPanel {
			b.Cache().PutPanelEmoji(panelID, r.emoji.ID.String())
		}
	}
	return nil
}

func (b *Bot) ensureEmoji(byName map[string]discord.Emoji, def EmojiDef) (discord.Emoji, error) {
	if emoji, exists := byName[def.Name]; exists {
		return emoji, nil
	}
	icon := discord.NewIconRaw(IconTypeAVIF, def.Image)
	created, err := b.Rest().CreateApplicationEmoji(b.ApplicationID(), discord.EmojiCreate{Name: def.Name, Image: *icon})
	if err != nil {
		return discord.Emoji{}, fmt.Errorf("sync emojis: upload %s: %w", def.Name, err)
	}
	slog.Info("sync emojis: uploaded", "name", def.Name, "id", created.ID)
	return *created, nil
}
