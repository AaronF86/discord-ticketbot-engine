package core

import "context"

type StepFn func(ctx context.Context, t *Ticket) error

type Step struct {
	Name string
	Run  StepFn
}

type Workflow struct {
	Steps []Step
}

type WorkflowState struct {
	CompletedSteps []string
}

func (w *Workflow) Execute(ctx context.Context, t *Ticket) error {
	completed := make(map[string]struct{}, len(t.WorkflowState.CompletedSteps))
	for _, s := range t.WorkflowState.CompletedSteps {
		completed[s] = struct{}{}
	}

	for _, step := range w.Steps {
		if _, done := completed[step.Name]; done {
			continue
		}
		if err := step.Run(ctx, t); err != nil {
			return err
		}
		t.WorkflowState.CompletedSteps = append(t.WorkflowState.CompletedSteps, step.Name)
	}
	return nil
}
