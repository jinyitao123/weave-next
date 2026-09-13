package workflow

// PublicationDraftRead is the locked workflow and exact mutable draft used to
// build a publication candidate in the caller-owned transaction.
type PublicationDraftRead struct {
	Workflow TeamWorkflow
	Draft    TeamWorkflowVersion
}
