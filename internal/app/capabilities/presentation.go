package capabilities

import "github.com/jinyitao123/weave/internal/kernel/capability"

// Return developer-owned labels and output selection, never internal prompts.
func presentInvocation(i *Invocation, d capability.Definition) {
	i.CapabilityName = d.Name
	i.StepNames = map[string]string{}
	outgoing := map[string]bool{}
	for _, relation := range d.Relations {
		outgoing[relation.From] = true
	}
	i.ResultSteps = []string{}
	for _, step := range d.Steps {
		i.StepNames[step.ID] = step.Name
		if !outgoing[step.ID] {
			i.ResultSteps = append(i.ResultSteps, step.ID)
		}
	}
}
