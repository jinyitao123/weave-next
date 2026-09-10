package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/freezer"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type CandidateInput struct {
	WorkspaceID     string
	WorkflowID      string
	WorkflowVersion int
}

type PublicationCandidate struct {
	WorkspaceID     string
	WorkflowID      string
	WorkflowVersion int

	ExpectedUpdatedAt time.Time

	ArtifactSchemaVersion     int
	CanonicalizationAlgorithm string
	CanonicalizationVersion   int
	HashAlgorithm             string
	ContentHash               string
	Payload                   frozen.ArtifactPayloadV1
	Dependencies              []TeamWorkflowDependency
}

type CandidateBuilder struct {
	workflows   *Store
	agents      *registry.AgentRegistry
	delivery    *delivery.Store
	skills      *skills.Store
	credentials *credentials.Store
	schedules   *schedule.Store
	descriptors *compiler.DescriptorRegistry
}

type candidateBundlePlan struct {
	reference    machine.ReferencedBundle
	record       registry.AgentRecord
	key          frozen.FactoryKey
	factoryInput []byte
	self         frozen.EnumeratedDependencyRef
}

func NewCandidateBuilder(
	workflows *Store,
	agents *registry.AgentRegistry,
	deliveryStore *delivery.Store,
	skillStore *skills.Store,
	credentialStore *credentials.Store,
	scheduleStore *schedule.Store,
	descriptors *compiler.DescriptorRegistry,
) *CandidateBuilder {
	return &CandidateBuilder{
		workflows: workflows, agents: agents, delivery: deliveryStore,
		skills: skillStore, credentials: credentialStore,
		schedules: scheduleStore, descriptors: descriptors,
	}
}

func (b *CandidateBuilder) BuildTx(
	ctx context.Context,
	tx pgx.Tx,
	input CandidateInput,
) (*PublicationCandidate, *machine.Report, error) {
	if b == nil || b.workflows == nil {
		return nil, nil, errors.New("build publication candidate: workflow store is required")
	}
	draft, err := b.workflows.ResolvePublicationDraftTx(
		ctx, tx, input.WorkspaceID, input.WorkflowID, input.WorkflowVersion,
	)
	if err != nil {
		return nil, nil, err
	}

	trigger, triggerReport := machine.DecodeTriggerConfigV1(draft.Draft.TriggerConfig)
	graph, graphReport := machine.DecodeGraphDefinitionV1(draft.Draft.GraphDefinition)
	var report machine.Report
	mergeCandidateReport(&report, triggerReport)
	mergeCandidateReport(&report, graphReport)
	if len(report.Issues) != 0 {
		return nil, &report, nil
	}
	if b.agents == nil || b.delivery == nil || b.skills == nil ||
		b.credentials == nil || b.schedules == nil || b.descriptors == nil {
		return nil, nil, errors.New("build publication candidate: dependencies are required")
	}

	team, err := b.agents.ResolvePublicationTeamTx(
		ctx, tx, input.WorkspaceID, draft.Workflow.TeamID,
	)
	if err != nil {
		return nil, nil, err
	}
	lead := machine.AgentVersionKey{
		AgentID: team.LeadAvatarID, AgentVersion: team.LeadAvatarVersion,
	}
	referenced := machine.ReferencedBundles(lead, graph)

	计划 := make([]candidateBundlePlan, len(referenced))
	credentialEncoder, err := credentials.NewTxEncoder(
		tx, input.WorkspaceID, map[frozen.CredentialKind]credentials.TxReferenceSource{
			frozen.CredentialProviderAPIKey:       b.credentials,
			frozen.CredentialDeliveryTargetAccess: b.delivery,
		},
	)
	if err != nil {
		return nil, nil, err
	}
	for index, reference := range referenced {
		version := reference.Key.AgentVersion
		record, resolveErr := b.agents.ResolveAgentVersionTx(
			ctx, tx, input.WorkspaceID, reference.Key.AgentID, &version,
		)
		if resolveErr != nil {
			return nil, nil, resolveErr
		}
		key, selectErr := b.descriptors.SelectAgentFactoryKey(*record)
		if selectErr != nil {
			return nil, nil, selectErr
		}
		if graphType := normalizedCandidateGraphType(record.GraphType); graphType != key.FactoryID {
			return nil, nil, compiler.ErrFactoryUnknown
		}
		descriptor, lookupErr := b.descriptors.Lookup(key)
		if lookupErr != nil {
			return nil, nil, lookupErr
		}
		factoryInput, encodeErr := descriptor.EnumerateDependencies.EncodeFactoryInput(
			ctx, *record, credentialEncoder,
		)
		if encodeErr != nil {
			return nil, nil, encodeErr
		}
		ownerVersion := reference.Key.AgentVersion
		计划[index] = candidateBundlePlan{
			reference: reference, record: *record, key: key, factoryInput: factoryInput,
			self: frozen.EnumeratedDependencyRef{
				WorkspaceID: input.WorkspaceID, OwnerType: "agent", OwnerID: reference.Key.AgentID,
				OwnerAgentVersion: &ownerVersion, DependencyType: "agent",
				DependencyKey: reference.Key.AgentID, DependencyVersion: &ownerVersion,
			},
		}
	}

	freezeResolver, err := freezer.BeginFreeze(ctx, tx, input.WorkspaceID, freezer.Sources{
		Agents: b.agents, Skills: b.skills, Providers: b.credentials, Delivery: b.delivery,
	})
	if err != nil {
		return nil, nil, err
	}
	workers, err := freezeResolver.ResolveTeamWorkersForShare(ctx, team.TeamID)
	if err != nil {
		return nil, nil, err
	}
	for index := range 计划 {
		usage := freezer.AgentUsageWorker
		if index == 0 {
			usage = freezer.AgentUsageLead
		}
		if _, err := freezeResolver.ResolveAgentVersion(
			ctx, 计划[index].self, usage, 计划[index].key, 计划[index].factoryInput,
		); err != nil {
			return nil, nil, err
		}
	}

	bundles := make([]frozen.FrozenExecutionBundle, 0, len(计划))
	bundlesByKey := make(map[machine.AgentVersionKey]frozen.FrozenExecutionBundle, len(计划))
	allEnumerated := make([]frozen.EnumeratedDependencyRef, 0, len(计划))
	var leadMetadata compiler.DependencyMetadata
	for _, plan := range 计划 {
		resolved, ok := freezeResolver.Lookup(plan.self)
		if !ok || resolved.Agent == nil {
			return nil, nil, errors.New("build publication candidate: frozen agent is unavailable")
		}
		selfMetadata, err := freezeResolver.ResolveMetadata(ctx, plan.self)
		if err != nil {
			return nil, nil, err
		}
		allEnumerated = append(allEnumerated, plan.self)
		if plan.reference.Key == lead {
			leadMetadata = selfMetadata
		}
		if !plan.reference.Executable {
			continue
		}
		descriptor, err := b.descriptors.Lookup(plan.key)
		if err != nil {
			return nil, nil, err
		}
		enumerated, err := descriptor.EnumerateDependencies.EnumerateDependencies(
			ctx, *resolved.Agent, freezeResolver,
		)
		if err != nil {
			return nil, nil, err
		}
		perAgentManifest, offlineResolver, err := freezer.ResolveManifest(
			ctx, enumerated, freezeResolver,
		)
		if err != nil {
			return nil, nil, err
		}
		bundle, err := buildCandidateBundle(*resolved.Agent, plan.key, perAgentManifest, freezeResolver)
		if err != nil {
			return nil, nil, err
		}
		bundleDependencies := append(
			[]frozen.FrozenDependencyRef(nil), perAgentManifest.Dependencies...,
		)
		bundleDependencies = append(bundleDependencies, frozen.FrozenDependencyRef{
			EnumeratedDependencyRef: selfMetadata.Ref,
			ContentHash:             selfMetadata.ContentHash,
		})
		_, bundle.Dependencies, err = frozen.BuildFrozenDependencyManifest(bundleDependencies)
		if err != nil {
			return nil, nil, err
		}
		_, capability, err := b.descriptors.ProduceCandidateCapability(
			ctx, plan.key, bundle, offlineResolver,
		)
		if err != nil {
			return nil, nil, err
		}
		bundle.Capability = capability
		if _, err := b.descriptors.CompileCandidate(ctx, plan.key, bundle, offlineResolver); err != nil {
			return nil, nil, err
		}
		bundles = append(bundles, bundle)
		bundlesByKey[plan.reference.Key] = bundle
		allEnumerated = append(allEnumerated, enumerated.Dependencies...)
	}
	if leadMetadata.ContentHash == "" {
		return nil, nil, errors.New("build publication candidate: frozen lead identity is unavailable")
	}
	globalManifest, _, err := freezer.ResolveManifest(ctx, frozen.EnumeratedDependencyManifest{
		SchemaVersion: frozen.FrozenSchemaVersion, Dependencies: allEnumerated,
	}, freezeResolver)
	if err != nil {
		return nil, nil, err
	}

	deliveryTargets, references := b.resolveCandidateReferences(ctx, tx, input.WorkspaceID, trigger)
	for _, resolution := range references {
		if resolution.err != nil && resolution.infrastructure {
			return nil, nil, resolution.err
		}
	}

	validation := candidateValidationContext(
		input.WorkspaceID, team, trigger, graph, 计划, bundlesByKey, references,
	)
	validated := machine.Validate(validation)
	mergeCandidateReport(&report, &validated)

	payload := frozen.ArtifactPayloadV1{
		SchemaVersion: frozen.ArtifactSchemaVersion,
		TriggerConfig: draft.Draft.TriggerConfig, GraphDefinition: draft.Draft.GraphDefinition,
		Team: frozen.ArtifactTeamV1{
			WorkspaceID: input.WorkspaceID, TeamID: team.TeamID,
			LeadAgentID: team.LeadAvatarID, LeadAgentVersion: int64(team.LeadAvatarVersion),
			LeadAgentContentHash: leadMetadata.ContentHash, Workers: workers,
		},
		Bundles: bundles, DeliveryTargets: deliveryTargets,
	}
	hashInput := frozen.ArtifactEnvelopeHashInputV1{
		WorkspaceID: input.WorkspaceID, WorkflowID: input.WorkflowID,
		WorkflowVersion:           input.WorkflowVersion,
		ArtifactSchemaVersion:     frozen.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm,
		CanonicalizationVersion:   frozen.ArtifactCanonicalizationVersion,
		HashAlgorithm:             frozen.ArtifactHashAlgorithm, Payload: payload,
	}
	contentHash, err := frozen.ComputeArtifactContentHash(hashInput)
	if err != nil {
		return nil, nil, err
	}
	dependencies, err := BuildCandidateDependencies(
		input.WorkspaceID, input.WorkflowID, input.WorkflowVersion,
		globalManifest, bundles, deliveryTargets,
	)
	if err != nil {
		return nil, nil, err
	}
	candidate := &PublicationCandidate{
		WorkspaceID: input.WorkspaceID, WorkflowID: input.WorkflowID,
		WorkflowVersion: input.WorkflowVersion, ExpectedUpdatedAt: draft.Draft.UpdatedAt,
		ArtifactSchemaVersion:     frozen.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm,
		CanonicalizationVersion:   frozen.ArtifactCanonicalizationVersion,
		HashAlgorithm:             frozen.ArtifactHashAlgorithm, ContentHash: contentHash,
		Payload: payload, Dependencies: dependencies,
	}
	return candidate, &report, nil
}

type candidateReferenceResolution struct {
	key            machine.ScopedReferenceKey
	state          machine.ProofState
	err            error
	infrastructure bool
}

func (b *CandidateBuilder) resolveCandidateReferences(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	trigger machine.TriggerConfig,
) ([]frozen.FrozenDeliveryTarget, []candidateReferenceResolution) {
	var targets []frozen.FrozenDeliveryTarget
	var resolutions []candidateReferenceResolution
	add := func(key machine.ScopedReferenceKey, state machine.ProofState, err error, infrastructure bool) {
		resolutions = append(resolutions, candidateReferenceResolution{
			key: key, state: state, err: err, infrastructure: infrastructure,
		})
	}
	switch config := trigger.Config.(type) {
	case machine.ConversationAutoConfig:
		state := machine.ProofNotFound
		if config.CatalogKey == "default" {
			state = machine.ProofResolved
		}
		add(machine.ScopedReferenceKey{Kind: machine.ScopedCatalog, ID: config.CatalogKey}, state, nil, false)
	case machine.ScheduleConfig:
		_, err := b.schedules.ResolveScheduleTx(ctx, tx, workspaceID, config.ScheduleID)
		state, infrastructure := candidateReferenceError(err, schedule.ErrScheduleNotFound)
		add(machine.ScopedReferenceKey{Kind: machine.ScopedSchedule, ID: config.ScheduleID}, state, err, infrastructure)
	}
	if trigger.Delivery != nil &&
		(trigger.Delivery.Kind == machine.DeliveryCallbackRef || trigger.Delivery.Kind == machine.DeliveryTargetRef) {
		target, err := b.delivery.ResolveDeliveryHeadTx(ctx, tx, workspaceID, trigger.Delivery.Ref)
		state, infrastructure := candidateDeliveryReferenceError(err)
		add(machine.ScopedReferenceKey{
			Kind: machine.ScopedDeliveryTarget, ID: trigger.Delivery.Ref,
		}, state, err, infrastructure)
		if err == nil {
			targets = append(targets, target)
		}
	}
	return targets, resolutions
}

func candidateReferenceError(err error, notFound error) (machine.ProofState, bool) {
	if err == nil {
		return machine.ProofResolved, false
	}
	if errors.Is(err, notFound) {
		return machine.ProofNotFound, false
	}
	return machine.ProofCorrupt, true
}

func candidateDeliveryReferenceError(err error) (machine.ProofState, bool) {
	if err == nil {
		return machine.ProofResolved, false
	}
	if errors.Is(err, delivery.ErrDependencyVersionRequired) || errors.Is(err, delivery.ErrTargetNotFound) {
		return machine.ProofNotFound, false
	}
	if errors.Is(err, delivery.ErrFrozenManifestMismatch) || errors.Is(err, delivery.ErrTargetClosed) {
		return machine.ProofCorrupt, false
	}
	return machine.ProofCorrupt, true
}

func normalizedCandidateGraphType(graphType string) string {
	if graphType == "" {
		return "standard"
	}
	return graphType
}

func buildCandidateBundle(
	agent frozen.FrozenAgentRecord,
	key frozen.FactoryKey,
	manifest frozen.FrozenDependencyManifest,
	resolver *freezer.Resolver,
) (frozen.FrozenExecutionBundle, error) {
	bundle := frozen.FrozenExecutionBundle{
		SchemaVersion: frozen.FrozenSchemaVersion, FactoryKey: key, Agent: agent,
		Skills: []frozen.FrozenSkill{}, MCPBindings: []frozen.FrozenMCPBinding{},
		FallbackModels: []frozen.FrozenModelBinding{}, Credentials: []frozen.CredentialReference{},
		Dependencies: manifest,
	}
	models := make(map[string]frozen.FrozenModelBinding)
	credentialSet := make(map[string]frozen.CredentialReference)
	addCredential := func(value frozen.CredentialReference) error {
		if err := frozen.ValidateCredentialReference(value); err != nil {
			return err
		}
		key := fmt.Sprintf("%s\x00%s\x00%s", value.Kind, value.ResourceID, value.Slot)
		credentialSet[key] = value
		return nil
	}
	for _, dependency := range manifest.Dependencies {
		resolved, ok := resolver.Lookup(dependency.EnumeratedDependencyRef)
		if !ok {
			return frozen.FrozenExecutionBundle{}, errors.New("build publication candidate: frozen dependency is unavailable")
		}
		switch {
		case resolved.Skill != nil:
			bundle.Skills = append(bundle.Skills, *resolved.Skill)
		case resolved.MCPBinding != nil:
			bundle.MCPBindings = append(bundle.MCPBindings, *resolved.MCPBinding)
			if err := addCredential(resolved.MCPBinding.AccessRef); err != nil {
				return frozen.FrozenExecutionBundle{}, err
			}
		case resolved.ModelBinding != nil:
			models[resolved.ModelBinding.ModelID] = *resolved.ModelBinding
			if err := addCredential(resolved.ModelBinding.CredentialRef); err != nil {
				return frozen.FrozenExecutionBundle{}, err
			}
		case resolved.RuntimeBinding != nil:
			value := *resolved.RuntimeBinding
			bundle.Runtime = &value
			if err := addCredential(value.AccessRef); err != nil {
				return frozen.FrozenExecutionBundle{}, err
			}
		case resolved.DeliveryTarget != nil:
			if err := addCredential(resolved.DeliveryTarget.AccessRef); err != nil {
				return frozen.FrozenExecutionBundle{}, err
			}
			for _, binding := range resolved.DeliveryTarget.CredentialBindings {
				if err := addCredential(binding.CredentialRef); err != nil {
					return frozen.FrozenExecutionBundle{}, err
				}
			}
		default:
			return frozen.FrozenExecutionBundle{}, errors.New("build publication candidate: frozen dependency payload is unavailable")
		}
	}
	if primary, ok := models[agent.Model]; ok {
		bundle.PrimaryModel = primary
	}
	for _, modelID := range agent.Fallback.Models {
		if agent.Engine != "loom" {
			continue
		}
		model, ok := models[modelID]
		if !ok {
			return frozen.FrozenExecutionBundle{}, errors.New("build publication candidate: fallback model binding is unavailable")
		}
		bundle.FallbackModels = append(bundle.FallbackModels, model)
	}
	credentialKeys := make([]string, 0, len(credentialSet))
	for key := range credentialSet {
		credentialKeys = append(credentialKeys, key)
	}
	sort.Strings(credentialKeys)
	for _, key := range credentialKeys {
		bundle.Credentials = append(bundle.Credentials, credentialSet[key])
	}
	return bundle, nil
}

func candidateValidationContext(
	workspaceID string,
	team *registry.PublicationTeamRead,
	trigger machine.TriggerConfig,
	graph machine.GraphDefinition,
	计划 []candidateBundlePlan,
	bundles map[machine.AgentVersionKey]frozen.FrozenExecutionBundle,
	references []candidateReferenceResolution,
) machine.ValidationContext {
	lead := machine.AgentVersionKey{AgentID: team.LeadAvatarID, AgentVersion: team.LeadAvatarVersion}
	workerProofs := make(map[string]machine.TeamWorkerProof, len(team.Workers))
	for _, worker := range team.Workers {
		kinds := make([]machine.AuthorizationKind, len(worker.AllowedKinds))
		for index, kind := range worker.AllowedKinds {
			kinds[index] = machine.AuthorizationKind(kind)
		}
		workerProofs[worker.WorkerAgentID] = machine.NewTeamWorkerProof(
			machine.ProofResolved, worker.Enabled, kinds,
		)
	}
	referenceProofs := make(map[machine.ScopedReferenceKey]machine.ProofState, len(references))
	for _, reference := range references {
		referenceProofs[reference.key] = reference.state
	}
	agents := make(map[machine.AgentVersionKey]machine.AgentVersionProof, len(计划))
	capabilities := make(map[machine.AgentVersionKey]machine.CapabilityProof, len(计划))
	dependencyProofs := make(map[machine.AgentVersionKey]machine.DependencyProof, len(计划))
	factoryProofs := make(map[machine.FactoryKey]machine.FactoryProof, len(计划))
	for _, plan := range 计划 {
		key := plan.reference.Key
		machineFactory := machine.FactoryKey{
			FactoryID: plan.key.FactoryID, FactoryVersion: plan.key.FactoryVersion,
			CompilerABI: plan.key.CompilerABI,
		}
		hasWorkerStep := false
		if plan.record.GraphDefinition != nil {
			for _, step := range plan.record.GraphDefinition.Steps {
				hasWorkerStep = hasWorkerStep || step.Type == "worker"
			}
		}
		var outputSchema []byte
		if plan.record.OutputSchema != nil {
			outputSchema = *plan.record.OutputSchema
		}
		agents[key] = machine.NewAgentVersionProof(
			machine.AgentProofResolved, machineFactory, outputSchema,
			len(plan.record.SubAgents) != 0 || len(plan.record.Spec.SubAgents) != 0,
			hasWorkerStep,
		)
		if bundle, executable := bundles[key]; executable {
			capability := bundle.Capability
			capabilities[key] = machine.NewCapabilityProof(
				machine.ProofResolved, capability.MayYield, capability.InteractiveStepIDs,
				capability.InteractiveToolIDs, capability.MayInvokeAgent, capability.AgentStepIDs,
			)
			dependencyProofs[key] = machine.NewDependencyProof(machine.DependencyProofResolved, nil)
			factoryProofs[machineFactory] = machine.NewFactoryProof(machine.FactoryProofResolved, nil)
		}
	}
	return machine.ValidationContext{
		WorkspaceID: workspaceID, TeamID: team.TeamID, Lead: lead,
		Trigger: trigger, Graph: graph,
		Authorization: machine.NewAuthorizationSnapshot(
			workspaceID, team.TeamID, machine.ProofResolved, workerProofs, referenceProofs,
		),
		Agents: agents, Capabilities: capabilities,
		Dependencies: machine.NewDependencySnapshot(
			dependencyProofs, machine.NewDependencyProof(machine.DependencyProofResolved, nil),
		),
		Factories: machine.NewFactorySnapshot(factoryProofs),
	}
}

func mergeCandidateReport(target *machine.Report, source *machine.Report) {
	if source == nil {
		return
	}
	for _, issue := range source.Issues {
		target.AddNode(issue.Phase, issue.Path, issue.NodeID, issue.Code, issue.Message)
	}
}
