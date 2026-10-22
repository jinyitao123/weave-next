package fanout

func NewWorkflowError(code ErrorCode, format string, args ...any) error {
	return workflowError(code, format, args...)
}
