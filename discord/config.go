package discord

type EscalationQueue struct {
	ID       string
	Name     string
	FGARoles []string
}

type Config struct {
	EscalationQueues []EscalationQueue
}

func (c Config) QueueByID(id string) EscalationQueue {
	for _, q := range c.EscalationQueues {
		if q.ID == id {
			return q
		}
	}
	return EscalationQueue{ID: id}
}
