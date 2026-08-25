package machine

type scopedReference struct {
	Key  ScopedReferenceKey
	Path string
}

func validateAuthorization(ctx ValidationContext) Report {
	var report Report
	snapshot := ctx.Authorization

	if ctx.WorkspaceID == "" || ctx.TeamID == "" ||
		snapshot.WorkspaceID == "" || snapshot.TeamID == "" ||
		snapshot.TeamState == "" {
		report.Add(
			PhaseAuthorization,
			"/authorization",
			CodeAuthorizationProofMissing,
			"authorization snapshot identity and team state are required",
		)
		report.Sort()
		return report
	}
	if snapshot.WorkspaceID != ctx.WorkspaceID {
		report.Add(
			PhaseAuthorization,
			"/authorization/workspace_id",
			CodeAuthorizationWorkspaceMismatch,
			"authorization snapshot workspace does not match validation context",
		)
		report.Sort()
		return report
	}
	if snapshot.TeamID != ctx.TeamID {
		report.Add(
			PhaseAuthorization,
			"/authorization/team_id",
			CodeAuthorizationWorkspaceMismatch,
			"authorization snapshot team does not match validation context",
		)
		report.Sort()
		return report
	}

	addTeamStateIssue(&report, snapshot.TeamState)
	validateNodeAuthorization(&report, ctx.Graph, snapshot)
	validateScopedReferences(&report, ctx.Trigger, snapshot)
	report.Sort()
	return report
}

func addTeamStateIssue(report *Report, state ProofState) {
	switch state {
	case ProofResolved:
	case ProofNotFound:
		report.Add(
			PhaseAuthorization,
			"/team_id",
			CodeTeamInactive,
			"owning team is not active",
		)
	case ProofCorrupt:
		report.Add(
			PhaseAuthorization,
			"/team_id",
			CodeAuthorizationCorrupt,
			"owning team authorization proof is corrupt",
		)
	case ProofUnprovable, ProofDeferredAgent, ProofDeferredDependencies, ProofDeferredFactory:
		report.Add(
			PhaseAuthorization,
			"/team_id",
			CodeAuthorizationUnprovable,
			"owning team authorization cannot be proven",
		)
	default:
		report.Add(
			PhaseAuthorization,
			"/team_id",
			CodeAuthorizationProofMissing,
			"owning team authorization proof is missing",
		)
	}
}

func validateNodeAuthorization(
	report *Report,
	graph GraphDefinition,
	snapshot AuthorizationSnapshot,
) {
	for index, node := range graph.Nodes {
		var agentID string
		var kind AuthorizationKind
		switch config := node.Config.(type) {
		case WorkerConfig:
			if node.Type != NodeWorker {
				continue
			}
			agentID = config.AgentID
			kind = AuthorizationKind(config.Kind)
		case HandoffConfig:
			if node.Type != NodeHandoff {
				continue
			}
			agentID = config.AgentID
			kind = AuthorizationHandoff
		default:
			continue
		}

		path := nodeConfigFieldPath(index, "agent_id")
		proof, found := snapshot.TeamWorker(agentID)
		if !found {
			report.AddNode(
				PhaseAuthorization,
				path,
				node.ID,
				CodeAuthorizationProofMissing,
				"TeamWorker authorization proof is missing",
			)
			continue
		}
		switch proof.State {
		case ProofResolved:
			if !proof.Enabled || !proof.allows(kind) {
				report.AddNode(
					PhaseAuthorization,
					path,
					node.ID,
					CodeNodeUnauthorized,
					"node target is not enabled for the requested team authorization kind",
				)
			}
		case ProofNotFound:
			report.AddNode(
				PhaseAuthorization,
				path,
				node.ID,
				CodeNodeUnauthorized,
				"node target is not a member of the owning team",
			)
		case ProofCorrupt:
			report.AddNode(
				PhaseAuthorization,
				path,
				node.ID,
				CodeAuthorizationCorrupt,
				"TeamWorker authorization proof is corrupt",
			)
		case ProofUnprovable, ProofDeferredAgent, ProofDeferredDependencies, ProofDeferredFactory:
			report.AddNode(
				PhaseAuthorization,
				path,
				node.ID,
				CodeAuthorizationUnprovable,
				"TeamWorker authorization cannot be proven",
			)
		default:
			report.AddNode(
				PhaseAuthorization,
				path,
				node.ID,
				CodeAuthorizationProofMissing,
				"TeamWorker authorization proof state is missing",
			)
		}
	}
}

func validateScopedReferences(
	report *Report,
	trigger TriggerConfig,
	snapshot AuthorizationSnapshot,
) {
	for _, reference := range scopedReferenceIndex(trigger) {
		state, found := snapshot.Reference(reference.Key)
		if !found {
			report.Add(
				PhaseAuthorization,
				reference.Path,
				CodeReferenceProofMissing,
				"workspace-scoped reference proof is missing",
			)
			continue
		}
		switch state {
		case ProofResolved:
		case ProofNotFound:
			report.Add(
				PhaseAuthorization,
				reference.Path,
				CodeReferenceNotFound,
				"workspace-scoped reference was not found",
			)
		case ProofCorrupt:
			report.Add(
				PhaseAuthorization,
				reference.Path,
				CodeReferenceCorrupt,
				"workspace-scoped reference is corrupt",
			)
		case ProofUnprovable, ProofDeferredAgent, ProofDeferredDependencies, ProofDeferredFactory:
			report.Add(
				PhaseAuthorization,
				reference.Path,
				CodeReferenceUnprovable,
				"workspace-scoped reference cannot be proven",
			)
		default:
			report.Add(
				PhaseAuthorization,
				reference.Path,
				CodeReferenceProofMissing,
				"workspace-scoped reference proof state is missing",
			)
		}
	}
}

func scopedReferenceIndex(trigger TriggerConfig) []scopedReference {
	references := make([]scopedReference, 0, 2)
	switch config := trigger.Config.(type) {
	case ConversationAutoConfig:
		if trigger.Type == TriggerConversationAuto {
			references = append(references, scopedReference{
				Key:  ScopedReferenceKey{Kind: ScopedCatalog, ID: config.CatalogKey},
				Path: "/trigger_config/config/catalog_key",
			})
		}
	case ScheduleConfig:
		if trigger.Type == TriggerSchedule {
			references = append(references, scopedReference{
				Key:  ScopedReferenceKey{Kind: ScopedSchedule, ID: config.ScheduleID},
				Path: "/trigger_config/config/schedule_id",
			})
		}
	}

	if trigger.Delivery != nil &&
		(trigger.Delivery.Kind == DeliveryCallbackRef || trigger.Delivery.Kind == DeliveryTargetRef) {
		references = append(references, scopedReference{
			Key: ScopedReferenceKey{
				Kind: ScopedDeliveryTarget,
				ID:   trigger.Delivery.Ref,
			},
			Path: "/trigger_config/delivery/ref",
		})
	}
	return references
}
