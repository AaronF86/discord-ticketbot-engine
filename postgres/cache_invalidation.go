package postgres

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	tb "git.aaronf86.tech/aaronf86/ticketbot/discord"
)

const cacheChannel = "ticketbot_cache"

const (
	kindButton     = "button"
	kindTemplate   = "template"
	kindPanelEmoji = "panel_emoji"
)

type CacheNotifier struct {
	pool *pgxpool.Pool
}

func NewCacheNotifier(pool *pgxpool.Pool) *CacheNotifier { return &CacheNotifier{pool: pool} }

func (n *CacheNotifier) InvalidateButton(actionType string)  { n.notify(kindButton, actionType) }
func (n *CacheNotifier) InvalidateTemplate(id string)        { n.notify(kindTemplate, id) }
func (n *CacheNotifier) InvalidatePanelEmoji(panelID string) { n.notify(kindPanelEmoji, panelID) }

func (n *CacheNotifier) notify(kind, key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := n.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, cacheChannel, kind+":"+key); err != nil {
		slog.Warn("cache invalidation notify failed", "kind", kind, "key", key, "err", err)
	}
}

func ListenCacheInvalidations(ctx context.Context, pool *pgxpool.Pool, cache *tb.Cache) {
	for ctx.Err() == nil {
		if err := listenOnce(ctx, pool, cache); err != nil && ctx.Err() == nil {
			slog.Warn("cache invalidation listener dropped, retrying", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func listenOnce(ctx context.Context, pool *pgxpool.Pool, cache *tb.Cache) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+cacheChannel); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		applyInvalidation(cache, n.Payload)
	}
}

func applyInvalidation(cache *tb.Cache, payload string) {
	kind, key, ok := strings.Cut(payload, ":")
	if !ok {
		return
	}
	switch kind {
	case kindButton:
		cache.InvalidateButton(key)
	case kindTemplate:
		cache.InvalidateTemplate(key)
	case kindPanelEmoji:
		cache.InvalidatePanelEmoji(key)
	}
}
