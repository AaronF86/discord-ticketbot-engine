package core

import "sync"

type Panel struct {
	ID          string
	Name        string
	Description string
	Workflow    *Workflow
	Metrics     []MetricSchema

	Vars map[string]string

	MaxTickets int

	DMTranscript bool
}

type PanelRegistry struct {
	mu     sync.RWMutex
	panels map[string]*Panel
}

func NewPanelRegistry() *PanelRegistry {
	return &PanelRegistry{panels: make(map[string]*Panel)}
}

func (r *PanelRegistry) Register(p *Panel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.panels[p.ID] = p
}

func (r *PanelRegistry) Get(id string) (*Panel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.panels[id]
	return p, ok
}

func (r *PanelRegistry) All() []*Panel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Panel, 0, len(r.panels))
	for _, p := range r.panels {
		out = append(out, p)
	}
	return out
}
