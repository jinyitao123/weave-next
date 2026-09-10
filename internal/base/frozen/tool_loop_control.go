package frozen

import "fmt"

// ValidateToolLoopControl keeps persisted counters exactly representable by all
// JSON consumers and the per-slice loop bounded before any execution effects.
func ValidateToolLoopControl(control *ToolLoopControl) error {
	if control == nil {
		return nil
	}
	if control.SliceRounds == 0 || control.SliceRounds > 1000 || control.InitialTotalRounds == 0 || control.InitialTotalRounds > 1<<53-1 {
		return fmt.Errorf("tool_loop_control requires slice_rounds in 1..1000 and initial_total_rounds in 1..9007199254740991")
	}
	return nil
}
