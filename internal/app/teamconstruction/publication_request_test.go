package teamconstruction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/publication"
)

func publicationCommandFixture(t *testing.T) (context.Context, PublicationCommand) {
	t.Helper()
	payload := frozen.ArtifactPayloadV1{SchemaVersion: 1, TriggerConfig: json.RawMessage(`{"type":"manual"}`),
		GraphDefinition: json.RawMessage(`{"schema_version":1,"nodes":[]}`), Team: frozen.ArtifactTeamV1{WorkspaceID: "workspace", TeamID: "team"}}
	encoded, err := frozen.Canonicalize(payload, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1,
		ArtifactSchemaVersion: 1, CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: 1, HashAlgorithm: "sha256", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	request := publication.PublishRequest{Version: publication.ContractVersion, RequestID: "publish-1", Candidate: frozen.ArtifactEnvelopeV1{
		WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1, ArtifactSchemaVersion: 1,
		CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: 1,
		HashAlgorithm: "sha256", ContentHash: hash, Payload: encoded}}
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	return ctx, PublicationCommand{Target: PublicationTarget{BuildRunID: "build", TeamID: "team", ExpectedAssetVersion: "asset-1"}, Request: request}
}

// This double models the required atomic boundaries, not SQL implementation.
// Real PostgreSQL adapters and production phase wiring are a separate batch.
type publicationBoundaryDouble struct {
	mu             sync.Mutex
	publication    *PublicationRequestRecord
	candidate      *CandidateRequestRecord
	published      map[string]publication.PublishReceipt
	admitted       map[string]publication.AdmissionReceipt
	publishCalls   int
	admitCalls     int
	activations    int
	failRevision   bool
	failAdmission  bool
	failActivation bool
}

func (s *publicationBoundaryDouble) ReservePublication(ctx context.Context, record PublicationRequestRecord) (PublicationRequestRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publication == nil {
		s.publication = &record
	}
	if err := s.publication.Verify(ctx, record.Command); err != nil {
		return PublicationRequestRecord{}, err
	}
	return *s.publication, nil
}

func (s *publicationBoundaryDouble) RecordRevision(ctx context.Context, record PublicationRequestRecord) (PublicationRequestRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failRevision {
		s.failRevision = false
		return PublicationRequestRecord{}, errors.New("receipt write interrupted")
	}
	if err := s.publication.Verify(ctx, record.Command); err != nil {
		return PublicationRequestRecord{}, err
	}
	next, err := s.publication.AcceptRevision(ctx, *record.Receipt)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	s.publication = &next
	return next, nil
}

func (s *publicationBoundaryDouble) ActivatePublication(ctx context.Context, record PublicationRequestRecord) (PublicationRequestRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failActivation {
		s.failActivation = false
		return PublicationRequestRecord{}, errors.New("asset authorization changed")
	}
	if err := s.publication.Verify(ctx, record.Command); err != nil {
		return PublicationRequestRecord{}, err
	}
	if s.publication.State != PublicationActivated {
		s.activations++
	}
	next, err := s.publication.Activated(ctx)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	s.publication = &next
	return next, nil
}

func (s *publicationBoundaryDouble) Publish(ctx context.Context, request publication.PublishRequest) (publication.PublishReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishCalls++
	digest, err := request.Fingerprint(ctx)
	if err != nil {
		return publication.PublishReceipt{}, err
	}
	if s.published == nil {
		s.published = map[string]publication.PublishReceipt{}
	}
	if prior, ok := s.published[request.RequestID]; ok {
		if err = prior.Verify(ctx, request); err != nil {
			return publication.PublishReceipt{}, err
		}
		return prior, nil
	}
	subject, _ := execution.RequireSubject(ctx, request.Candidate.WorkspaceID)
	receipt := publication.PublishReceipt{Version: publication.ContractVersion, RequestID: request.RequestID, RequestDigest: digest, Subject: subject, Revision: publication.CandidateRevision(request.Candidate)}
	s.published[request.RequestID] = receipt
	return receipt, nil
}

func (s *publicationBoundaryDouble) ReserveCandidate(ctx context.Context, record CandidateRequestRecord) (CandidateRequestRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.candidate == nil {
		s.candidate = &record
	}
	if err := s.candidate.Verify(ctx, record.Target, record.Request); err != nil {
		return CandidateRequestRecord{}, err
	}
	return *s.candidate, nil
}

func (s *publicationBoundaryDouble) RecordAdmission(ctx context.Context, record CandidateRequestRecord) (CandidateRequestRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAdmission {
		s.failAdmission = false
		return CandidateRequestRecord{}, errors.New("admission receipt interrupted")
	}
	if err := s.candidate.Verify(ctx, record.Target, record.Request); err != nil {
		return CandidateRequestRecord{}, err
	}
	if s.candidate.Receipt != nil && *s.candidate.Receipt != *record.Receipt {
		return CandidateRequestRecord{}, publication.ErrRequestConflict
	}
	s.candidate = &record
	return record, nil
}

func (s *publicationBoundaryDouble) AdmitCandidate(ctx context.Context, request publication.CandidateRunRequest) (publication.AdmissionReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admitCalls++
	digest, err := request.Fingerprint(ctx)
	if err != nil {
		return publication.AdmissionReceipt{}, err
	}
	if s.admitted == nil {
		s.admitted = map[string]publication.AdmissionReceipt{}
	}
	if prior, ok := s.admitted[request.RequestID]; ok {
		if err = prior.Verify(ctx, request); err != nil {
			return publication.AdmissionReceipt{}, err
		}
		return prior, nil
	}
	subject, _ := execution.RequireSubject(ctx, request.Candidate.WorkspaceID)
	receipt := publication.AdmissionReceipt{Version: publication.ContractVersion, RequestID: request.RequestID, RequestDigest: digest, Subject: subject,
		Revision: publication.CandidateRevision(request.Candidate), TaskID: "task-1", RunID: "run-1", RunSnapshotID: "snapshot-1"}
	s.admitted[request.RequestID] = receipt
	return receipt, nil
}

func TestPublicationRequestInterruptedReceiptReplaysSameRevision(t *testing.T) {
	ctx, command := publicationCommandFixture(t)
	store := &publicationBoundaryDouble{failRevision: true}
	flow := PublicationFlow{Requests: store, Kernel: store}
	if _, err := flow.Publish(ctx, command); err == nil {
		t.Fatal("expected interrupted receipt persistence")
	}
	if store.publication.State != PublicationPending || len(store.published) != 1 || store.activations != 0 {
		t.Fatalf("wrong persisted boundary: %+v", store)
	}
	// A reconstructed caller resumes the durable request, not a builder call.
	flow = PublicationFlow{Requests: store, Kernel: store}
	result, err := flow.Publish(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != PublicationActivated || len(store.published) != 1 || store.activations != 1 || store.publishCalls != 2 {
		t.Fatalf("duplicate publication or lost activation: %+v", store)
	}
}

func TestPublicationRequestActivationFailureRetainsRevision(t *testing.T) {
	ctx, command := publicationCommandFixture(t)
	store := &publicationBoundaryDouble{failActivation: true}
	flow := PublicationFlow{Requests: store, Kernel: store}
	if _, err := flow.Publish(ctx, command); err == nil {
		t.Fatal("expected activation denial")
	}
	if store.publication.State != PublicationRevisionObtained {
		t.Fatal(store.publication.State)
	}
	if _, err := flow.Publish(ctx, command); err != nil {
		t.Fatal(err)
	}
	if store.publishCalls != 1 || store.activations != 1 {
		t.Fatal("retry repeated kernel publication")
	}
}

func TestPublicationRequestConcurrentRetriesAndIdentityFences(t *testing.T) {
	ctx, command := publicationCommandFixture(t)
	store := &publicationBoundaryDouble{}
	flow := PublicationFlow{Requests: store, Kernel: store}
	var wg sync.WaitGroup
	errorsCh := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := flow.Publish(ctx, command); errorsCh <- err }()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(store.published) != 1 || store.activations != 1 {
		t.Fatal("duplicate durable effect")
	}
	other := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "bob"})
	if _, err := flow.Publish(other, command); !errors.Is(err, execution.ErrSubjectMismatch) {
		t.Fatalf("foreign request accepted: %v", err)
	}
	changed := command
	changed.Target.ExpectedAssetVersion = "asset-2"
	if _, err := flow.Publish(ctx, changed); !errors.Is(err, publication.ErrRequestConflict) {
		t.Fatalf("changed activation target accepted: %v", err)
	}
	if _, err := flow.Publish(context.Background(), command); !errors.Is(err, execution.ErrSubjectRequired) {
		t.Fatalf("missing actor accepted: %v", err)
	}
}

func TestPublicationRequestRejectsSkippedAndMismatchedTransitions(t *testing.T) {
	ctx, command := publicationCommandFixture(t)
	record, err := NewPublicationRequest(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = record.Activated(ctx); !errors.Is(err, ErrPublicationTransition) {
		t.Fatalf("activation before revision accepted: %v", err)
	}
	store := &publicationBoundaryDouble{}
	receipt, err := store.Publish(ctx, command.Request)
	if err != nil {
		t.Fatal(err)
	}
	bad := receipt
	bad.Subject.UserID = "bob"
	if _, err = record.AcceptRevision(ctx, bad); !errors.Is(err, publication.ErrInvalidReceipt) {
		t.Fatalf("foreign receipt accepted: %v", err)
	}
	bad = receipt
	bad.Revision.ContentHash = fmt.Sprintf("%064d", 0)
	if _, err = record.AcceptRevision(ctx, bad); !errors.Is(err, publication.ErrInvalidReceipt) {
		t.Fatalf("different frozen content accepted: %v", err)
	}
	record, err = record.AcceptRevision(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	record, err = record.Activated(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := record.AcceptRevision(ctx, receipt)
	if err != nil || repeated.State != PublicationActivated {
		t.Fatalf("late receipt regressed activation: %v", err)
	}
}

func TestCandidateAdmissionInterruptedReceiptNeverAdmitsAnotherTask(t *testing.T) {
	ctx, command := publicationCommandFixture(t)
	deadline := time.Now().UTC().Add(time.Minute)
	request := publication.CandidateRunRequest{Version: publication.ContractVersion, RequestID: "candidate-1", Candidate: command.Request.Candidate,
		Input: json.RawMessage(`{"a":1,"b":2}`), InputVersion: "input-1", SourceRef: "build-1", Purpose: "evaluation-1", ParentTaskID: "parent-1", DeadlineAt: &deadline}
	store := &publicationBoundaryDouble{failAdmission: true}
	flow := CandidateAdmissionFlow{Requests: store, Kernel: store}
	target := CandidateTarget{BuildRunID: "build-1", RoundNo: 1, SourceRole: "evaluation"}
	if _, err := flow.Admit(ctx, target, request); err == nil {
		t.Fatal("expected interrupted receipt write")
	}
	if store.candidate.Receipt != nil || len(store.admitted) != 1 {
		t.Fatal("wrong admission boundary")
	}
	receipt, err := flow.Admit(ctx, target, request)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Receipt.TaskID != "task-1" || len(store.admitted) != 1 || store.admitCalls != 2 {
		t.Fatal("duplicate admission")
	}
	changed := request
	changed.Input = json.RawMessage(`{"a":2,"b":2}`)
	if _, err = flow.Admit(ctx, target, changed); !errors.Is(err, publication.ErrRequestConflict) {
		t.Fatalf("input replaced during recovery: %v", err)
	}
	changed = request
	later := deadline.Add(time.Minute)
	changed.DeadlineAt = &later
	if _, err = flow.Admit(ctx, target, changed); !errors.Is(err, publication.ErrRequestConflict) {
		t.Fatalf("deadline renewed by replay: %v", err)
	}
	request.Input = json.RawMessage("{\n \"b\":2, \"a\":1 }")
	if _, err = flow.Admit(ctx, target, request); err != nil {
		t.Fatal("equivalent JSON failed idempotency:", err)
	}
	if store.admitCalls != 2 {
		t.Fatal("stored receipt replay called kernel again")
	}
	other := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "bob"})
	if _, err = flow.Admit(other, target, request); !errors.Is(err, execution.ErrSubjectMismatch) {
		t.Fatalf("foreign candidate request accepted: %v", err)
	}
}

func TestPublicationContractRejectsTamperedEnvelopeAndVersions(t *testing.T) {
	ctx, command := publicationCommandFixture(t)
	request := command.Request
	request.Version = "weave.publication/v2"
	if _, err := request.Fingerprint(ctx); !errors.Is(err, publication.ErrInvalidRequest) {
		t.Fatal("unknown contract version accepted")
	}
	request = command.Request
	request.Candidate.Payload = json.RawMessage(`{}`)
	if _, err := request.Fingerprint(ctx); err == nil {
		t.Fatal("invalid envelope accepted")
	}
	request = command.Request
	request.Candidate.ContentHash = fmt.Sprintf("%064d", 0)
	if _, err := request.Fingerprint(ctx); err == nil {
		t.Fatal("mismatched content hash accepted")
	}
}
