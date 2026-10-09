package discord

import "sync"

type Cache struct {
	buttons     sync.Map
	templates   sync.Map
	panelEmojis sync.Map
}

func NewCache() *Cache { return &Cache{} }

type cachedButton struct {
	Label string
	Style int32
	Emoji string
}

type cachedTemplate struct {
	LayoutData   []byte
	RequiredPerm string
}

func (c *Cache) Button(actionType string) (ButtonConfig, bool) {
	v, ok := c.buttons.Load(actionType)
	if !ok {
		return ButtonConfig{}, false
	}
	cb := v.(cachedButton)
	return ButtonConfig(cb), true
}

func (c *Cache) PutButton(actionType string, cfg ButtonConfig) {
	c.buttons.Store(actionType, cachedButton(cfg))
}

func (c *Cache) InvalidateButton(actionType string) { c.buttons.Delete(actionType) }

func (c *Cache) Template(id string) (TemplateLayout, bool) {
	v, ok := c.templates.Load(id)
	if !ok {
		return TemplateLayout{}, false
	}
	ct := v.(cachedTemplate)
	return TemplateLayout(ct), true
}

func (c *Cache) PutTemplate(id string, layout TemplateLayout) {
	c.templates.Store(id, cachedTemplate(layout))
}

func (c *Cache) InvalidateTemplate(id string) { c.templates.Delete(id) }

func (c *Cache) PanelEmoji(panelID string) string {
	v, ok := c.panelEmojis.Load(panelID)
	if !ok {
		return ""
	}
	return v.(string)
}

func (c *Cache) PutPanelEmoji(panelID, emojiID string) { c.panelEmojis.Store(panelID, emojiID) }

func (c *Cache) InvalidatePanelEmoji(panelID string) { c.panelEmojis.Delete(panelID) }
