package core

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
	panels map[string]*Panel
}

func NewPanelRegistry() *PanelRegistry {
	return &PanelRegistry{panels: make(map[string]*Panel)}
}

func (r *PanelRegistry) Register(p *Panel) {
	r.panels[p.ID] = p
}

func (r *PanelRegistry) Get(id string) (*Panel, bool) {
	p, ok := r.panels[id]
	return p, ok
}

func (r *PanelRegistry) All() []*Panel {
	out := make([]*Panel, 0, len(r.panels))
	for _, p := range r.panels {
		out = append(out, p)
	}
	return out
}
