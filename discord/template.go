package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"

	"git.aaronf86.tech/aaronf86/ticketbot/core"
)

type TemplateService struct {
	repo  TemplateRepository
	cache *Cache
}

func NewTemplateService(repo TemplateRepository, cache ...*Cache) *TemplateService {
	svc := &TemplateService{repo: repo}
	if len(cache) > 0 {
		svc.cache = cache[0]
	}
	return svc
}

func (s *TemplateService) layout(ctx context.Context, id string) (TemplateLayout, error) {
	if s.cache != nil {
		if layout, ok := s.cache.Template(id); ok {
			return layout, nil
		}
	}
	layout, err := s.repo.GetTemplateLayout(ctx, id)
	if err != nil {
		return TemplateLayout{}, err
	}
	if s.cache != nil {
		s.cache.PutTemplate(id, layout)
	}
	return layout, nil
}

func (s *TemplateService) List(ctx context.Context) ([]TemplateInfo, error) {
	return s.repo.ListTemplates(ctx)
}

func (s *TemplateService) Compile(ctx context.Context, id string, vars map[string]string) ([]discord.LayoutComponent, error) {
	layout, err := s.layout(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("template %q: %w", id, err)
	}
	if len(layout.LayoutData) == 0 {
		return nil, fmt.Errorf("template %q: layout_data is empty", id)
	}

	raw := string(layout.LayoutData)
	for k, v := range vars {
		raw = strings.ReplaceAll(raw, "{"+k+"}", v)
		raw = strings.ReplaceAll(raw, "<<"+k+">>", v)
	}

	return unmarshalComponents(raw)
}

func (s *TemplateService) CompileWithPanels(ctx context.Context, id string, vars map[string]string, panels []*core.Panel) ([]discord.LayoutComponent, string, error) {
	layout, err := s.layout(ctx, id)
	if err != nil {
		return nil, "", fmt.Errorf("template %q: %w", id, err)
	}
	if len(layout.LayoutData) == 0 {
		return nil, "", fmt.Errorf("template %q: layout_data is empty", id)
	}

	raw := string(layout.LayoutData)
	for k, v := range vars {
		raw = strings.ReplaceAll(raw, "{"+k+"}", v)
	}

	emojiIDs, err := s.panelEmojiIDs(ctx, panels)
	if err != nil {
		return nil, "", fmt.Errorf("template %q: load panel emojis: %w", id, err)
	}

	raw, err = injectPanelOptions(raw, panels, emojiIDs)
	if err != nil {
		return nil, "", fmt.Errorf("template %q: inject panel options: %w", id, err)
	}

	components, err := unmarshalComponents(raw)
	if err != nil {
		return nil, "", err
	}
	return components, layout.RequiredPerm, nil
}

func (s *TemplateService) panelEmojiIDs(ctx context.Context, panels []*core.Panel) (map[string]string, error) {
	out := make(map[string]string, len(panels))
	missing := s.fillCachedEmojis(panels, out)
	if len(missing) == 0 {
		return out, nil
	}

	actionTypes := make([]string, len(missing))
	for i, id := range missing {
		actionTypes[i] = "panel:" + id
	}
	emojis, err := s.repo.ListButtonEmojis(ctx, actionTypes)
	if err != nil {
		return nil, err
	}
	for _, row := range emojis {
		panelID := strings.TrimPrefix(row.ActionType, "panel:")
		out[panelID] = row.Emoji
		if s.cache != nil {
			s.cache.PutPanelEmoji(panelID, row.Emoji)
		}
	}
	return out, nil
}

func (s *TemplateService) fillCachedEmojis(panels []*core.Panel, out map[string]string) (missing []string) {
	for _, p := range panels {
		if s.cache != nil {
			if id := s.cache.PanelEmoji(p.ID); id != "" {
				out[p.ID] = id
				continue
			}
		}
		missing = append(missing, p.ID)
	}
	return missing
}

const componentTypeTextDisplay = 10

// pruneEmptyText removes text displays with empty content, and any
// container or section left childless as a result. Discord rejects the whole
// message when a text display's content is empty.
func pruneEmptyText(nodes []any) []any {
	out := make([]any, 0, len(nodes))
	for _, node := range nodes {
		m, ok := node.(map[string]any)
		if !ok {
			out = append(out, node)
			continue
		}
		if t, _ := m["type"].(float64); t == componentTypeTextDisplay {
			if c, _ := m["content"].(string); strings.TrimSpace(c) == "" {
				continue
			}
		}
		if children, ok := m["components"].([]any); ok && len(children) > 0 {
			kept := pruneEmptyText(children)
			if len(kept) == 0 {
				continue
			}
			m["components"] = kept
		}
		out = append(out, m)
	}
	return out
}

func unmarshalComponents(raw string) ([]discord.LayoutComponent, error) {
	var tree []any
	if err := json.Unmarshal([]byte(raw), &tree); err != nil {
		return nil, fmt.Errorf("unmarshal components: %w", err)
	}
	pruned, err := json.Marshal(pruneEmptyText(tree))
	if err != nil {
		return nil, fmt.Errorf("unmarshal components: %w", err)
	}

	var components []discord.UnmarshalComponent
	if err := json.Unmarshal(pruned, &components); err != nil {
		return nil, fmt.Errorf("unmarshal components: %w", err)
	}
	out := make([]discord.LayoutComponent, 0, len(components))
	for _, c := range components {
		if lc, ok := c.Component.(discord.LayoutComponent); ok {
			out = append(out, lc)
		}
	}
	return out, nil
}

func injectPanelOptions(raw string, panels []*core.Panel, emojiIDs map[string]string) (string, error) {
	var tree []any
	if err := json.Unmarshal([]byte(raw), &tree); err != nil {
		return raw, err
	}
	injectIntoNode(tree, panels, emojiIDs)
	out, err := json.Marshal(tree)
	if err != nil {
		return raw, err
	}
	return string(out), nil
}

func injectIntoNode(nodes []any, panels []*core.Panel, emojiIDs map[string]string) {
	for _, node := range nodes {
		m, ok := node.(map[string]any)
		if !ok {
			continue
		}
		if children, ok := m["components"].([]any); ok {
			injectIntoNode(children, panels, emojiIDs)
		}
		t, _ := m["type"].(float64)
		cid, _ := m["custom_id"].(string)
		if t == 3 && cid == "panel:create" {
			m["options"] = panelOptions(panels, emojiIDs)
		}
	}
}

func panelOptions(panels []*core.Panel, emojiIDs map[string]string) []any {
	opts := make([]any, 0, len(panels))
	for _, p := range panels {
		opt := map[string]any{
			"label":       p.Name,
			"value":       p.ID,
			"description": p.Description,
		}
		if id := emojiIDs[p.ID]; id != "" {
			opt["emoji"] = map[string]any{"id": id}
		}
		opts = append(opts, opt)
	}
	return opts
}
