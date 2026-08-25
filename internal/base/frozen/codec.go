package frozen

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

var (
	ErrFrozenSchemaVersion       = errors.New("frozen schema_version must be 1")
	ErrFrozenDuplicateSetValue   = errors.New("frozen set contains a duplicate value")
	ErrFrozenSchemaMismatch      = errors.New("value does not match preorder schema")
	ErrFrozenContentHashMismatch = errors.New("frozen content hash mismatch")
	ErrFrozenManifestOrder       = errors.New("frozen dependency manifest is not canonically ordered")
	ErrFrozenDuplicateDependency = errors.New("frozen dependency manifest contains a duplicate identity")
	ErrFrozenCredentialInvalid   = errors.New("frozen credential reference is invalid")
	ErrFrozenRuntimeAmbiguous    = errors.New("loom agent must not carry a runtime_id or runtime binding")
	ErrFrozenRuntimeRequired     = errors.New("CLI agent requires a matching runtime binding")
	ErrFrozenCapabilityIdentity  = errors.New("frozen capability role and agent content hash are required")
)

const CodeArtifactInvalid = "workflow_artifact_invalid"

var ErrArtifactInvalid = &ArtifactError{}

type ArtifactError struct {
	cause error
}

func (e *ArtifactError) Error() string {
	if e == nil {
		return ""
	}
	if e.cause == nil {
		return CodeArtifactInvalid
	}
	return CodeArtifactInvalid + ": " + e.cause.Error()
}

func (e *ArtifactError) Code() string {
	if e == nil {
		return ""
	}
	return CodeArtifactInvalid
}

func (e *ArtifactError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *ArtifactError) Is(target error) bool {
	other, ok := target.(*ArtifactError)
	return ok && e != nil && other != nil
}

func artifactInvalid(cause error) error {
	if errors.Is(cause, ErrArtifactInvalid) {
		return cause
	}
	return &ArtifactError{cause: cause}
}

type PreorderSchema string

const (
	PreorderFrozenAgentRecord            PreorderSchema = "frozen_agent_record_v1"
	PreorderFrozenSkill                  PreorderSchema = "frozen_skill_v1"
	PreorderFrozenSkillResource          PreorderSchema = "frozen_skill_resource_v1"
	PreorderFrozenMCPBinding             PreorderSchema = "frozen_mcp_binding_v1"
	PreorderFrozenModelBinding           PreorderSchema = "frozen_model_binding_v1"
	PreorderFrozenRuntimeBinding         PreorderSchema = "frozen_runtime_binding_v1"
	PreorderCredentialReference          PreorderSchema = "credential_reference_v1"
	PreorderFrozenDeliveryTarget         PreorderSchema = "frozen_delivery_target_v1"
	PreorderFrozenTeamWorker             PreorderSchema = "frozen_team_worker_v1"
	PreorderEnumeratedDependencyManifest PreorderSchema = "enumerated_dependency_manifest_v1"
	PreorderFrozenDependencyManifest     PreorderSchema = "frozen_dependency_manifest_v1"
	PreorderCapabilityManifest           PreorderSchema = "capability_manifest_v2"
	PreorderTeamInteractionCatalog       PreorderSchema = "team_interaction_catalog_v1"
	PreorderFrozenExecutionBundle        PreorderSchema = "frozen_execution_bundle_v1"
	PreorderArtifactPayloadV1            PreorderSchema = "artifact_payload_v1"
	PreorderArtifactEnvelopeHashInputV1  PreorderSchema = "artifact_envelope_hash_input_v1"
)

func DecodeFrozenAgentRecord(raw []byte) (FrozenAgentRecord, error) {
	value, err := strictDecode[FrozenAgentRecord](raw)
	if err != nil {
		return FrozenAgentRecord{}, err
	}
	return NormalizeFrozenAgentRecord(value)
}

func DecodeFrozenSkill(raw []byte) (FrozenSkill, error) {
	value, err := strictDecode[FrozenSkill](raw)
	if err != nil {
		return FrozenSkill{}, err
	}
	return normalizeFrozenSkill(value)
}

func DecodeFrozenSkillResource(raw []byte) (FrozenSkillResource, error) {
	value, err := strictDecode[FrozenSkillResource](raw)
	if err != nil {
		return FrozenSkillResource{}, err
	}
	if err := validateFrozenSkillResource(value); err != nil {
		return FrozenSkillResource{}, err
	}
	return value, nil
}

func DecodeFrozenMCPBinding(raw []byte) (FrozenMCPBinding, error) {
	value, err := strictDecode[FrozenMCPBinding](raw)
	if err != nil {
		return FrozenMCPBinding{}, err
	}
	return normalizeFrozenMCPBinding(value)
}

func DecodeFrozenModelBinding(raw []byte) (FrozenModelBinding, error) {
	value, err := strictDecode[FrozenModelBinding](raw)
	if err != nil {
		return FrozenModelBinding{}, err
	}
	return normalizeFrozenModelBinding(value)
}

func DecodeFrozenRuntimeBinding(raw []byte) (FrozenRuntimeBinding, error) {
	value, err := strictDecode[FrozenRuntimeBinding](raw)
	if err != nil {
		return FrozenRuntimeBinding{}, err
	}
	return normalizeFrozenRuntimeBinding(value)
}

func DecodeCredentialReference(raw []byte) (CredentialReference, error) {
	value, err := strictDecode[CredentialReference](raw)
	if err != nil {
		return CredentialReference{}, err
	}
	if err := ValidateCredentialReference(value); err != nil {
		return CredentialReference{}, err
	}
	return value, nil
}

func DecodeFrozenDeliveryTarget(raw []byte) (FrozenDeliveryTarget, error) {
	value, err := strictDecode[FrozenDeliveryTarget](raw)
	if err != nil {
		return FrozenDeliveryTarget{}, err
	}
	return normalizeFrozenDeliveryTarget(value)
}

func DecodeFrozenTeamWorker(raw []byte) (FrozenTeamWorker, error) {
	value, err := strictDecode[FrozenTeamWorker](raw)
	if err != nil {
		return FrozenTeamWorker{}, err
	}
	return normalizeFrozenTeamWorker(value)
}

func DecodeEnumeratedDependencyManifest(raw []byte) (EnumeratedDependencyManifest, error) {
	value, err := strictDecode[EnumeratedDependencyManifest](raw)
	if err != nil {
		return EnumeratedDependencyManifest{}, err
	}
	return normalizeEnumeratedDependencyManifest(value)
}

func DecodeFrozenDependencyManifest(raw []byte) (FrozenDependencyManifest, error) {
	value, err := strictDecode[FrozenDependencyManifest](raw)
	if err != nil {
		return FrozenDependencyManifest{}, err
	}
	if err := ValidateManifest(value); err != nil {
		return FrozenDependencyManifest{}, err
	}
	return value, nil
}

func DecodeCapabilityManifest(raw []byte) (CapabilityManifest, error) {
	value, err := strictDecode[CapabilityManifest](raw)
	if err != nil {
		return CapabilityManifest{}, err
	}
	return normalizeCapabilityManifest(value)
}

func DecodeFrozenExecutionBundle(raw []byte) (FrozenExecutionBundle, error) {
	value, err := strictDecode[FrozenExecutionBundle](raw)
	if err != nil {
		return FrozenExecutionBundle{}, err
	}
	return normalizeFrozenExecutionBundle(value)
}

func DecodeArtifactPayloadV1(raw []byte) (ArtifactPayloadV1, error) {
	value, err := strictDecode[ArtifactPayloadV1](raw)
	if err != nil {
		return ArtifactPayloadV1{}, artifactInvalid(err)
	}
	return normalizeArtifactPayloadV1(value)
}

func DecodeArtifactEnvelopeV1(envelope ArtifactEnvelopeV1) (ArtifactPayloadV1, error) {
	if envelope.WorkspaceID == "" || envelope.WorkflowID == "" || envelope.WorkflowVersion < 1 {
		return ArtifactPayloadV1{}, artifactInvalid(ErrFrozenSchemaMismatch)
	}
	if envelope.ArtifactSchemaVersion != ArtifactSchemaVersion {
		return ArtifactPayloadV1{}, artifactInvalid(ErrFrozenSchemaVersion)
	}
	if envelope.CanonicalizationAlgorithm != ArtifactCanonicalizationAlgorithm ||
		envelope.CanonicalizationVersion != ArtifactCanonicalizationVersion ||
		envelope.HashAlgorithm != ArtifactHashAlgorithm {
		return ArtifactPayloadV1{}, artifactInvalid(ErrFrozenSchemaMismatch)
	}
	if !validLowerHexHash(envelope.ContentHash) {
		return ArtifactPayloadV1{}, artifactInvalid(ErrFrozenContentHashMismatch)
	}
	payload, err := DecodeArtifactPayloadV1(envelope.Payload)
	if err != nil {
		return ArtifactPayloadV1{}, artifactInvalid(err)
	}
	computed, err := ComputeArtifactContentHash(ArtifactEnvelopeHashInputV1{
		WorkspaceID:               envelope.WorkspaceID,
		WorkflowID:                envelope.WorkflowID,
		WorkflowVersion:           envelope.WorkflowVersion,
		ArtifactSchemaVersion:     envelope.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: envelope.CanonicalizationAlgorithm,
		CanonicalizationVersion:   envelope.CanonicalizationVersion,
		HashAlgorithm:             envelope.HashAlgorithm,
		Payload:                   payload,
	})
	if err != nil {
		return ArtifactPayloadV1{}, artifactInvalid(err)
	}
	if subtle.ConstantTimeCompare([]byte(computed), []byte(envelope.ContentHash)) != 1 {
		return ArtifactPayloadV1{}, artifactInvalid(ErrFrozenContentHashMismatch)
	}
	return payload, nil
}

func strictDecode[T any](raw []byte) (T, error) {
	var zero T
	if err := preflightSafeIntegerFields(raw); err != nil {
		return zero, err
	}
	canonical, err := canonicalizeStrictJSON(raw)
	if err != nil {
		return zero, err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	var value T
	if err := decoder.Decode(&value); err != nil {
		return zero, fmt.Errorf("decode frozen DTO: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return zero, err
	}
	return value, nil
}

func preflightSafeIntegerFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanJSONValueForSafeIntegers(decoder, "", true); err != nil {
		if errors.Is(err, ErrJCSSafeIntegerRange) {
			return err
		}
		// Strict JCS owns syntax, Unicode, duplicate-key, and trailing-value
		// classification. This preflight only prevents numeric precision loss.
		return nil
	}
	return nil
}

func scanJSONValueForSafeIntegers(
	decoder *json.Decoder,
	field string,
	enforceIdentityFields bool,
) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch typed := token.(type) {
	case json.Delim:
		switch typed {
		case '{':
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key is not a string")
				}
				enforceChild := enforceIdentityFields &&
					key != "output_schema" &&
					key != "factory_input" &&
					key != "trigger_config" &&
					key != "graph_definition"
				if err := scanJSONValueForSafeIntegers(decoder, key, enforceChild); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := scanJSONValueForSafeIntegers(
					decoder,
					"",
					enforceIdentityFields,
				); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		}
	case json.Number:
		if enforceIdentityFields && safeIntegerIdentityField(field) {
			value, parseErr := strconv.ParseInt(string(typed), 10, 64)
			if parseErr != nil {
				return ErrJCSSafeIntegerRange
			}
			return validateJCSSafeInteger(value)
		}
	}
	return nil
}

func safeIntegerIdentityField(field string) bool {
	switch field {
	case "schema_version",
		"agent_version",
		"skill_version",
		"server_revision",
		"provider_revision",
		"runtime_revision",
		"target_revision",
		"credential_version",
		"owner_agent_version",
		"dependency_version":
		return true
	default:
		return false
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("decode frozen DTO: trailing JSON value")
	}
	return fmt.Errorf("decode frozen DTO trailing data: %w", err)
}

func Canonicalize(value any, schema PreorderSchema) ([]byte, error) {
	normalized, err := normalizedForSchema(value, schema)
	if err != nil {
		return nil, err
	}
	canonical, err := canonicalizeTyped(normalized)
	if err != nil && isArtifactPreorder(schema) {
		return nil, artifactInvalid(err)
	}
	return canonical, err
}

func HashDTO(value any, schema PreorderSchema) (string, error) {
	normalized, err := normalizedForSchema(value, schema)
	if err != nil {
		return "", err
	}

	switch typed := normalized.(type) {
	case FrozenSkillResource:
		return ComputeResourceContentHash(typed)
	case FrozenDependencyManifest:
		return ComputeManifestHash(typed.Dependencies)
	case FrozenSkill:
		normalized = frozenSkillHashInput{
			SchemaVersion: typed.SchemaVersion,
			WorkspaceID:   typed.WorkspaceID, Name: typed.Name, SourceType: typed.SourceType,
			SkillID: typed.SkillID, SkillVersion: cloneInt64Pointer(typed.SkillVersion),
			Description: typed.Description, Body: typed.Body, AlwaysActive: typed.AlwaysActive,
			Resources: typed.Resources,
		}
	case FrozenMCPBinding:
		normalized = frozenMCPHashInput{
			SchemaVersion: typed.SchemaVersion, WorkspaceID: typed.WorkspaceID,
			ServerID: typed.ServerID, ServerRevision: typed.ServerRevision,
			Transport: typed.Transport, URL: typed.URL, Command: typed.Command,
			Args: typed.Args, Filter: typed.Filter, WriteTools: typed.WriteTools,
			AccessRef: typed.AccessRef,
		}
	case FrozenModelBinding:
		normalized = frozenModelHashInput{
			SchemaVersion: typed.SchemaVersion, WorkspaceID: typed.WorkspaceID,
			ProviderID: typed.ProviderID, ProviderRevision: typed.ProviderRevision,
			ModelID: typed.ModelID, BaseURL: typed.BaseURL,
			JSONObjectMode: typed.JSONObjectMode, CredentialRef: typed.CredentialRef,
		}
	case FrozenRuntimeBinding:
		normalized = frozenRuntimeHashInput{
			SchemaVersion: typed.SchemaVersion, WorkspaceID: typed.WorkspaceID,
			RuntimeID: typed.RuntimeID, Engine: typed.Engine,
			RuntimeRevision: typed.RuntimeRevision, AccessRef: typed.AccessRef,
		}
	case FrozenDeliveryTarget:
		normalized = frozenDeliveryHashInput{
			SchemaVersion: typed.SchemaVersion, WorkspaceID: typed.WorkspaceID,
			TargetID: typed.TargetID, TargetRevision: typed.TargetRevision,
			Kind: typed.Kind, Transport: typed.Transport, URL: typed.URL,
			Method: typed.Method, ContentType: typed.ContentType,
			TimeoutSeconds:     typed.TimeoutSeconds,
			CredentialBindings: typed.CredentialBindings, AccessRef: typed.AccessRef,
		}
	}

	canonical, err := canonicalizeTyped(normalized)
	if err != nil {
		return "", err
	}
	return sha256Hex(canonical), nil
}

func normalizedForSchema(value any, schema PreorderSchema) (any, error) {
	if err := validateValueUTF8(value); err != nil {
		if isArtifactPreorder(schema) {
			return nil, artifactInvalid(err)
		}
		return nil, err
	}
	switch schema {
	case PreorderFrozenAgentRecord:
		typed, ok := value.(FrozenAgentRecord)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		return NormalizeFrozenAgentRecord(typed)
	case PreorderFrozenSkill:
		typed, ok := value.(FrozenSkill)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		return normalizeFrozenSkill(typed)
	case PreorderFrozenSkillResource:
		typed, ok := value.(FrozenSkillResource)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		if err := validateFrozenSkillResource(typed); err != nil {
			return nil, err
		}
		return typed, nil
	case PreorderFrozenMCPBinding:
		typed, ok := value.(FrozenMCPBinding)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		return normalizeFrozenMCPBinding(typed)
	case PreorderFrozenModelBinding:
		typed, ok := value.(FrozenModelBinding)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		return normalizeFrozenModelBinding(typed)
	case PreorderFrozenRuntimeBinding:
		typed, ok := value.(FrozenRuntimeBinding)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		return normalizeFrozenRuntimeBinding(typed)
	case PreorderCredentialReference:
		typed, ok := value.(CredentialReference)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		if err := ValidateCredentialReference(typed); err != nil {
			return nil, err
		}
		return cloneTyped(typed)
	case PreorderFrozenDeliveryTarget:
		typed, ok := value.(FrozenDeliveryTarget)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		return normalizeFrozenDeliveryTarget(typed)
	case PreorderFrozenTeamWorker:
		typed, ok := value.(FrozenTeamWorker)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		return normalizeFrozenTeamWorker(typed)
	case PreorderEnumeratedDependencyManifest:
		typed, ok := value.(EnumeratedDependencyManifest)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		return normalizeEnumeratedDependencyManifest(typed)
	case PreorderFrozenDependencyManifest:
		typed, ok := value.(FrozenDependencyManifest)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		if err := ValidateManifest(typed); err != nil {
			return nil, err
		}
		return cloneTyped(typed)
	case PreorderCapabilityManifest:
		typed, ok := value.(CapabilityManifest)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		return normalizeCapabilityManifest(typed)
	case PreorderTeamInteractionCatalog:
		typed, ok := value.(TeamInteractionCatalog)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		return normalizeTeamInteractionCatalog(typed)
	case PreorderFrozenExecutionBundle:
		typed, ok := value.(FrozenExecutionBundle)
		if !ok {
			return nil, ErrFrozenSchemaMismatch
		}
		return normalizeFrozenExecutionBundle(typed)
	case PreorderArtifactPayloadV1:
		typed, ok := value.(ArtifactPayloadV1)
		if !ok {
			return nil, artifactInvalid(ErrFrozenSchemaMismatch)
		}
		return normalizeArtifactPayloadV1(typed)
	case PreorderArtifactEnvelopeHashInputV1:
		typed, ok := value.(ArtifactEnvelopeHashInputV1)
		if !ok {
			return nil, artifactInvalid(ErrFrozenSchemaMismatch)
		}
		return normalizeArtifactEnvelopeHashInputV1(typed)
	default:
		return nil, ErrFrozenSchemaMismatch
	}
}

func NormalizeFrozenAgentRecord(value FrozenAgentRecord) (FrozenAgentRecord, error) {
	var err error
	if value.OutputSchema, err = canonicalNullableRawJSON(value.OutputSchema); err != nil {
		return FrozenAgentRecord{}, err
	}
	if value.FactoryInput, err = canonicalRequiredJSONObject(value.FactoryInput); err != nil {
		return FrozenAgentRecord{}, err
	}
	cloned, err := cloneTyped(value)
	if err != nil {
		return FrozenAgentRecord{}, err
	}
	if err := requireSchemaVersion(cloned.SchemaVersion); err != nil {
		return FrozenAgentRecord{}, err
	}
	if err := validateJCSSafeInteger(cloned.AgentVersion); err != nil {
		return FrozenAgentRecord{}, err
	}
	if math.IsNaN(cloned.Limits.MaxCostUSD) || math.IsInf(cloned.Limits.MaxCostUSD, 0) {
		return FrozenAgentRecord{}, errors.New("frozen max_cost_usd must be finite")
	}

	if cloned.SystemPrompt != "" && cloned.Identity.Raw != "" &&
		cloned.SystemPrompt != cloned.Identity.Raw {
		return FrozenAgentRecord{}, errors.New("frozen system_prompt and identity.raw conflict")
	}
	if cloned.SystemPrompt == "" {
		cloned.SystemPrompt = cloned.Identity.Raw
	}
	if cloned.Identity.Raw == "" {
		cloned.Identity.Raw = cloned.SystemPrompt
	}
	if cloned.Identity.Core == "" {
		cloned.Identity.Core = cloned.SystemPrompt
	}
	if cloned.Engine == "" {
		cloned.Engine = "loom"
	}
	if cloned.GraphType == "" {
		cloned.GraphType = "standard"
	}

	if cloned.Profiles == nil {
		cloned.Profiles = map[string]FrozenAgentProfile{}
	}
	if cloned.Permissions.Deny, err = canonicalStringSet(cloned.Permissions.Deny); err != nil {
		return FrozenAgentRecord{}, err
	}
	if cloned.Permissions.Allow, err = canonicalStringSet(cloned.Permissions.Allow); err != nil {
		return FrozenAgentRecord{}, err
	}
	if cloned.Permissions.Ask, err = canonicalStringSet(cloned.Permissions.Ask); err != nil {
		return FrozenAgentRecord{}, err
	}
	if cloned.MemorySlots, err = canonicalMemorySlots(cloned.MemorySlots); err != nil {
		return FrozenAgentRecord{}, err
	}
	cloned.Fallback.Models = normalizeOrderedStrings(cloned.Fallback.Models)
	if cloned.Guard != nil {
		if cloned.Guard.BlockedTerms, err = canonicalStringSet(cloned.Guard.BlockedTerms); err != nil {
			return FrozenAgentRecord{}, err
		}
	}
	if cloned.GraphType == "standard" && string(cloned.FactoryInput) != "{}" {
		return FrozenAgentRecord{}, errors.New("standard factory_input must be an empty object")
	}
	return cloned, nil
}

func normalizeFrozenSkill(value FrozenSkill) (FrozenSkill, error) {
	cloned, err := cloneTyped(value)
	if err != nil {
		return FrozenSkill{}, err
	}
	if err := requireSchemaVersion(cloned.SchemaVersion); err != nil {
		return FrozenSkill{}, err
	}
	if cloned.SkillVersion != nil {
		if err := validateJCSSafeInteger(*cloned.SkillVersion); err != nil {
			return FrozenSkill{}, err
		}
	}
	if cloned.Resources == nil {
		cloned.Resources = []FrozenSkillResource{}
	}
	for _, resource := range cloned.Resources {
		if err := validateFrozenSkillResource(resource); err != nil {
			return FrozenSkill{}, err
		}
	}
	sort.Slice(cloned.Resources, func(i, j int) bool {
		return compareResource(cloned.Resources[i], cloned.Resources[j]) < 0
	})
	for index := 1; index < len(cloned.Resources); index++ {
		if cloned.Resources[index-1].Kind == cloned.Resources[index].Kind &&
			cloned.Resources[index-1].RelativePath == cloned.Resources[index].RelativePath {
			return FrozenSkill{}, ErrFrozenDuplicateSetValue
		}
	}
	return cloned, nil
}

func normalizeFrozenMCPBinding(value FrozenMCPBinding) (FrozenMCPBinding, error) {
	cloned, err := cloneTyped(value)
	if err != nil {
		return FrozenMCPBinding{}, err
	}
	if err := requireSchemaVersion(cloned.SchemaVersion); err != nil {
		return FrozenMCPBinding{}, err
	}
	if err := validateJCSSafeInteger(cloned.ServerRevision); err != nil {
		return FrozenMCPBinding{}, err
	}
	cloned.Args = normalizeOrderedStrings(cloned.Args)
	if cloned.Filter, err = canonicalStringSet(cloned.Filter); err != nil {
		return FrozenMCPBinding{}, err
	}
	if cloned.WriteTools, err = canonicalStringSet(cloned.WriteTools); err != nil {
		return FrozenMCPBinding{}, err
	}
	if err := ValidateCredentialReference(cloned.AccessRef); err != nil ||
		cloned.AccessRef.Kind != CredentialMCPServerAccess ||
		cloned.AccessRef.WorkspaceID != cloned.WorkspaceID ||
		cloned.AccessRef.ResourceID != cloned.ServerID ||
		cloned.AccessRef.Slot != "access" {
		return FrozenMCPBinding{}, ErrFrozenCredentialInvalid
	}
	switch cloned.Transport {
	case "http":
		if cloned.URL == "" || cloned.Command != "" || len(cloned.Args) != 0 {
			return FrozenMCPBinding{}, errors.New("invalid frozen HTTP MCP transport contract")
		}
		if err := validateManagedHTTPURL(cloned.URL); err != nil {
			return FrozenMCPBinding{}, err
		}
	case "stdio":
		if cloned.URL != "" || cloned.Command == "" {
			return FrozenMCPBinding{}, errors.New("invalid frozen stdio MCP transport contract")
		}
	default:
		return FrozenMCPBinding{}, errors.New("unsupported frozen MCP transport")
	}
	return cloned, nil
}

func normalizeFrozenModelBinding(value FrozenModelBinding) (FrozenModelBinding, error) {
	cloned, err := cloneTyped(value)
	if err != nil {
		return FrozenModelBinding{}, err
	}
	if err := requireSchemaVersion(cloned.SchemaVersion); err != nil {
		return FrozenModelBinding{}, err
	}
	if err := validateJCSSafeInteger(cloned.ProviderRevision); err != nil {
		return FrozenModelBinding{}, err
	}
	if err := validateManagedHTTPURL(cloned.BaseURL); err != nil {
		return FrozenModelBinding{}, err
	}
	if err := ValidateCredentialReference(cloned.CredentialRef); err != nil ||
		cloned.CredentialRef.Kind != CredentialProviderAPIKey ||
		cloned.CredentialRef.WorkspaceID != cloned.WorkspaceID ||
		cloned.CredentialRef.ResourceID != cloned.ProviderID ||
		cloned.CredentialRef.Slot != "api_key" {
		return FrozenModelBinding{}, ErrFrozenCredentialInvalid
	}
	return cloned, nil
}

func normalizeFrozenRuntimeBinding(value FrozenRuntimeBinding) (FrozenRuntimeBinding, error) {
	cloned, err := cloneTyped(value)
	if err != nil {
		return FrozenRuntimeBinding{}, err
	}
	if err := requireSchemaVersion(cloned.SchemaVersion); err != nil {
		return FrozenRuntimeBinding{}, err
	}
	if err := validateJCSSafeInteger(cloned.RuntimeRevision); err != nil {
		return FrozenRuntimeBinding{}, err
	}
	if cloned.Engine == "loom" {
		return FrozenRuntimeBinding{}, ErrFrozenRuntimeAmbiguous
	}
	if err := ValidateCredentialReference(cloned.AccessRef); err != nil ||
		cloned.AccessRef.Kind != CredentialRuntimeAccess ||
		cloned.AccessRef.WorkspaceID != cloned.WorkspaceID ||
		cloned.AccessRef.ResourceID != cloned.RuntimeID ||
		cloned.AccessRef.Slot != "access" {
		return FrozenRuntimeBinding{}, ErrFrozenCredentialInvalid
	}
	return cloned, nil
}

func ValidateCredentialReference(value CredentialReference) error {
	if err := requireSchemaVersion(value.SchemaVersion); err != nil {
		return err
	}
	if value.WorkspaceID == "" || value.ResourceID == "" || value.CredentialVersion != nil {
		return ErrFrozenCredentialInvalid
	}
	switch value.Kind {
	case CredentialProviderAPIKey:
		if value.Slot != "api_key" {
			return ErrFrozenCredentialInvalid
		}
	case CredentialMCPServerAccess, CredentialRuntimeAccess:
		if value.Slot != "access" {
			return ErrFrozenCredentialInvalid
		}
	case CredentialDeliveryTargetAccess:
		if value.Slot == "access" {
			return nil
		}
		const prefix = "header:"
		if !strings.HasPrefix(value.Slot, prefix) ||
			!validLowercaseHTTPToken(strings.TrimPrefix(value.Slot, prefix)) {
			return ErrFrozenCredentialInvalid
		}
	default:
		return ErrFrozenCredentialInvalid
	}
	return nil
}

func normalizeFrozenDeliveryTarget(value FrozenDeliveryTarget) (FrozenDeliveryTarget, error) {
	cloned, err := cloneTyped(value)
	if err != nil {
		return FrozenDeliveryTarget{}, err
	}
	if err := requireSchemaVersion(cloned.SchemaVersion); err != nil {
		return FrozenDeliveryTarget{}, err
	}
	if err := validateJCSSafeInteger(cloned.TargetRevision); err != nil {
		return FrozenDeliveryTarget{}, err
	}
	if cloned.Kind != "callback" && cloned.Kind != "target" {
		return FrozenDeliveryTarget{}, errors.New("invalid frozen delivery kind")
	}
	if cloned.Transport != "http" || cloned.Method != "POST" ||
		cloned.ContentType != "application/json" {
		return FrozenDeliveryTarget{}, errors.New("invalid frozen delivery transport contract")
	}
	if cloned.TimeoutSeconds < 1 || cloned.TimeoutSeconds > 300 {
		return FrozenDeliveryTarget{}, errors.New("frozen delivery timeout must be between 1 and 300 seconds")
	}
	if err := validateManagedHTTPURL(cloned.URL); err != nil {
		return FrozenDeliveryTarget{}, err
	}
	if cloned.CredentialBindings == nil {
		cloned.CredentialBindings = []FrozenDeliveryCredentialBinding{}
	}
	for _, binding := range cloned.CredentialBindings {
		if !validLowercaseHTTPToken(binding.HeaderName) ||
			binding.CredentialRef.Kind != CredentialDeliveryTargetAccess ||
			binding.CredentialRef.Slot != "header:"+binding.HeaderName {
			return FrozenDeliveryTarget{}, ErrFrozenCredentialInvalid
		}
		if err := ValidateCredentialReference(binding.CredentialRef); err != nil {
			return FrozenDeliveryTarget{}, err
		}
		if binding.CredentialRef.WorkspaceID != cloned.WorkspaceID ||
			binding.CredentialRef.ResourceID != cloned.TargetID {
			return FrozenDeliveryTarget{}, ErrFrozenCredentialInvalid
		}
	}
	sort.Slice(cloned.CredentialBindings, func(i, j int) bool {
		return compareUTF16Strings(
			cloned.CredentialBindings[i].HeaderName,
			cloned.CredentialBindings[j].HeaderName,
		) < 0
	})
	for index := 1; index < len(cloned.CredentialBindings); index++ {
		if cloned.CredentialBindings[index-1].HeaderName ==
			cloned.CredentialBindings[index].HeaderName {
			return FrozenDeliveryTarget{}, ErrFrozenDuplicateSetValue
		}
	}
	if err := ValidateCredentialReference(cloned.AccessRef); err != nil ||
		cloned.AccessRef.Kind != CredentialDeliveryTargetAccess ||
		cloned.AccessRef.Slot != "access" ||
		cloned.AccessRef.WorkspaceID != cloned.WorkspaceID ||
		cloned.AccessRef.ResourceID != cloned.TargetID {
		return FrozenDeliveryTarget{}, ErrFrozenCredentialInvalid
	}
	return cloned, nil
}

func normalizeFrozenTeamWorker(value FrozenTeamWorker) (FrozenTeamWorker, error) {
	cloned, err := cloneTyped(value)
	if err != nil {
		return FrozenTeamWorker{}, err
	}
	if err := requireSchemaVersion(cloned.SchemaVersion); err != nil {
		return FrozenTeamWorker{}, err
	}
	if cloned.AllowedKinds, err = canonicalStringSet(cloned.AllowedKinds); err != nil {
		return FrozenTeamWorker{}, err
	}
	if len(cloned.AllowedKinds) == 0 {
		return FrozenTeamWorker{}, errors.New("frozen TeamWorker allowed_kinds must not be empty")
	}
	allowedDefault := false
	for _, kind := range cloned.AllowedKinds {
		switch kind {
		case "consult", "dispatch", "handoff":
		default:
			return FrozenTeamWorker{}, errors.New("invalid frozen TeamWorker kind")
		}
		if kind == cloned.DefaultKind {
			allowedDefault = true
		}
	}
	if !allowedDefault {
		return FrozenTeamWorker{}, errors.New("frozen TeamWorker default_kind is not allowed")
	}
	return cloned, nil
}

func normalizeEnumeratedDependencyManifest(
	value EnumeratedDependencyManifest,
) (EnumeratedDependencyManifest, error) {
	cloned, err := cloneTyped(value)
	if err != nil {
		return EnumeratedDependencyManifest{}, err
	}
	if err := requireSchemaVersion(cloned.SchemaVersion); err != nil {
		return EnumeratedDependencyManifest{}, err
	}
	if cloned.Dependencies == nil {
		cloned.Dependencies = []EnumeratedDependencyRef{}
	}
	for _, dependency := range cloned.Dependencies {
		if err := validateEnumeratedDependencyRef(dependency); err != nil {
			return EnumeratedDependencyManifest{}, err
		}
	}
	sort.Slice(cloned.Dependencies, func(i, j int) bool {
		return compareEnumeratedDependency(
			cloned.Dependencies[i],
			cloned.Dependencies[j],
		) < 0
	})
	if hasDuplicateEnumeratedDependencies(cloned.Dependencies) {
		return EnumeratedDependencyManifest{}, ErrFrozenDuplicateDependency
	}
	return cloned, nil
}

func normalizeCapabilityManifest(value CapabilityManifest) (CapabilityManifest, error) {
	cloned, err := cloneTyped(value)
	if err != nil {
		return CapabilityManifest{}, err
	}
	if cloned.SchemaVersion != CapabilityManifestSchemaVersion {
		return CapabilityManifest{}, fmt.Errorf(
			"frozen capability schema_version must be %d: %w",
			CapabilityManifestSchemaVersion,
			ErrFrozenSchemaVersion,
		)
	}
	if cloned.Role == "" || !validLowerHexHash(cloned.AgentContentHash) {
		return CapabilityManifest{}, ErrFrozenCapabilityIdentity
	}
	if cloned.InteractiveStepIDs, err = canonicalStringSet(cloned.InteractiveStepIDs); err != nil {
		return CapabilityManifest{}, err
	}
	if cloned.InteractiveToolIDs, err = canonicalStringSet(cloned.InteractiveToolIDs); err != nil {
		return CapabilityManifest{}, err
	}
	if cloned.AgentStepIDs, err = canonicalStringSet(cloned.AgentStepIDs); err != nil {
		return CapabilityManifest{}, err
	}
	return cloned, nil
}

func normalizeTeamInteractionCatalog(value TeamInteractionCatalog) (TeamInteractionCatalog, error) {
	cloned, err := cloneTyped(value)
	if err != nil {
		return TeamInteractionCatalog{}, err
	}
	if cloned.WorkspaceID == "" || cloned.TeamID == "" || cloned.RunID == "" ||
		cloned.RunSnapshotID == "" || cloned.LeadAgentID == "" {
		return TeamInteractionCatalog{}, ErrFrozenSchemaMismatch
	}
	if cloned.Consult == nil {
		cloned.Consult = map[string]TeamInteractionAuthorizedWorker{}
	}
	if cloned.Dispatch == nil {
		cloned.Dispatch = map[string]TeamInteractionAuthorizedWorker{}
	}
	if cloned.Handoff == nil {
		cloned.Handoff = map[string]TeamInteractionHandoffRoute{}
	}
	for key, worker := range cloned.Consult {
		if err := validateTeamInteractionWorker(key, worker); err != nil {
			return TeamInteractionCatalog{}, err
		}
	}
	for key, worker := range cloned.Dispatch {
		if err := validateTeamInteractionWorker(key, worker); err != nil {
			return TeamInteractionCatalog{}, err
		}
	}
	for key, route := range cloned.Handoff {
		if key == "" || key != route.RouteKey || route.WorkerAgentID == "" ||
			route.WorkerAgentVersion < 1 || !validTeamInteractionRouteKey(key) ||
			key != canonicalTeamInteractionRouteKey(cloned.RunSnapshotID, route.WorkerAgentID) {
			return TeamInteractionCatalog{}, ErrFrozenSchemaMismatch
		}
		if err := validateJCSSafeInteger(route.WorkerAgentVersion); err != nil {
			return TeamInteractionCatalog{}, err
		}
	}
	return cloned, nil
}

func validateTeamInteractionWorker(key string, worker TeamInteractionAuthorizedWorker) error {
	if key == "" || key != worker.WorkerAgentID || worker.WorkerAgentVersion < 1 {
		return ErrFrozenSchemaMismatch
	}
	if err := validateJCSSafeInteger(worker.WorkerAgentVersion); err != nil {
		return err
	}
	switch worker.DefaultKind {
	case "consult", "dispatch", "handoff":
		return nil
	default:
		return ErrFrozenSchemaMismatch
	}
}

func validTeamInteractionRouteKey(value string) bool {
	if !strings.HasPrefix(value, "tw1.") || len(value) == len("tw1.") {
		return false
	}
	for _, char := range value {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func canonicalTeamInteractionRouteKey(runSnapshotID, workerAgentID string) string {
	snapshotHash := sha256.Sum256([]byte(runSnapshotID))
	return "tw1." + base64.RawURLEncoding.EncodeToString(snapshotHash[:]) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(workerAgentID))
}

func normalizeFrozenExecutionBundle(
	value FrozenExecutionBundle,
) (FrozenExecutionBundle, error) {
	cloned, err := cloneTyped(value)
	if err != nil {
		return FrozenExecutionBundle{}, err
	}
	if err := requireSchemaVersion(cloned.SchemaVersion); err != nil {
		return FrozenExecutionBundle{}, err
	}
	if cloned.FactoryKey.FactoryID == "" || cloned.FactoryKey.FactoryVersion == "" ||
		cloned.FactoryKey.CompilerABI == "" {
		return FrozenExecutionBundle{}, errors.New("frozen FactoryKey is incomplete")
	}
	if cloned.Agent, err = NormalizeFrozenAgentRecord(cloned.Agent); err != nil {
		return FrozenExecutionBundle{}, err
	}
	if cloned.Agent.GraphType != cloned.FactoryKey.FactoryID {
		return FrozenExecutionBundle{}, errors.New("agent graph_type does not match FactoryKey")
	}
	if cloned.Skills == nil {
		cloned.Skills = []FrozenSkill{}
	}
	for index := range cloned.Skills {
		if cloned.Skills[index], err = normalizeFrozenSkill(cloned.Skills[index]); err != nil {
			return FrozenExecutionBundle{}, err
		}
	}
	sort.Slice(cloned.Skills, func(i, j int) bool {
		return compareSkill(cloned.Skills[i], cloned.Skills[j]) < 0
	})
	for index := 1; index < len(cloned.Skills); index++ {
		if compareSkill(cloned.Skills[index-1], cloned.Skills[index]) == 0 {
			return FrozenExecutionBundle{}, ErrFrozenDuplicateSetValue
		}
	}
	if cloned.MCPBindings == nil {
		cloned.MCPBindings = []FrozenMCPBinding{}
	}
	for index := range cloned.MCPBindings {
		if cloned.MCPBindings[index], err =
			normalizeFrozenMCPBinding(cloned.MCPBindings[index]); err != nil {
			return FrozenExecutionBundle{}, err
		}
	}
	sort.Slice(cloned.MCPBindings, func(i, j int) bool {
		return compareUTF16Strings(
			cloned.MCPBindings[i].ServerID,
			cloned.MCPBindings[j].ServerID,
		) < 0
	})
	for index := 1; index < len(cloned.MCPBindings); index++ {
		if cloned.MCPBindings[index-1].ServerID == cloned.MCPBindings[index].ServerID {
			return FrozenExecutionBundle{}, ErrFrozenDuplicateSetValue
		}
	}
	if cloned.Agent.Model == "" {
		if cloned.PrimaryModel != (FrozenModelBinding{}) {
			return FrozenExecutionBundle{}, ErrFrozenSchemaMismatch
		}
	} else {
		if cloned.PrimaryModel, err = normalizeFrozenModelBinding(cloned.PrimaryModel); err != nil {
			return FrozenExecutionBundle{}, err
		}
		if cloned.PrimaryModel.ModelID != cloned.Agent.Model {
			return FrozenExecutionBundle{}, ErrFrozenSchemaMismatch
		}
	}
	if cloned.FallbackModels == nil {
		cloned.FallbackModels = []FrozenModelBinding{}
	}
	for index := range cloned.FallbackModels {
		if cloned.FallbackModels[index], err =
			normalizeFrozenModelBinding(cloned.FallbackModels[index]); err != nil {
			return FrozenExecutionBundle{}, err
		}
	}
	if cloned.Runtime != nil {
		runtime, runtimeErr := normalizeFrozenRuntimeBinding(*cloned.Runtime)
		if runtimeErr != nil {
			return FrozenExecutionBundle{}, runtimeErr
		}
		cloned.Runtime = &runtime
	}
	if cloned.Credentials == nil {
		cloned.Credentials = []CredentialReference{}
	}
	for _, credential := range cloned.Credentials {
		if err := ValidateCredentialReference(credential); err != nil {
			return FrozenExecutionBundle{}, err
		}
	}
	sort.Slice(cloned.Credentials, func(i, j int) bool {
		return compareCredential(cloned.Credentials[i], cloned.Credentials[j]) < 0
	})
	for index := 1; index < len(cloned.Credentials); index++ {
		if compareCredential(cloned.Credentials[index-1], cloned.Credentials[index]) == 0 {
			return FrozenExecutionBundle{}, ErrFrozenDuplicateSetValue
		}
	}
	if err := ValidateManifest(cloned.Dependencies); err != nil {
		return FrozenExecutionBundle{}, err
	}
	if cloned.Capability, err = normalizeCapabilityManifest(cloned.Capability); err != nil {
		return FrozenExecutionBundle{}, err
	}
	if cloned.Agent.Engine == "loom" {
		if cloned.Agent.RuntimeID != "" || cloned.Runtime != nil {
			return FrozenExecutionBundle{}, ErrFrozenRuntimeAmbiguous
		}
	} else {
		if cloned.Agent.RuntimeID == "" || cloned.Runtime == nil ||
			cloned.Runtime.RuntimeID != cloned.Agent.RuntimeID ||
			cloned.Runtime.Engine != cloned.Agent.Engine {
			return FrozenExecutionBundle{}, ErrFrozenRuntimeRequired
		}
	}
	return cloned, nil
}

func ValidateFrozenExecutionBundle(value FrozenExecutionBundle) error {
	_, err := normalizeFrozenExecutionBundle(value)
	return err
}

func normalizeArtifactPayloadV1(value ArtifactPayloadV1) (ArtifactPayloadV1, error) {
	if err := validateValueUTF8(value); err != nil {
		return ArtifactPayloadV1{}, artifactInvalid(err)
	}
	cloned, err := cloneTyped(value)
	if err != nil {
		return ArtifactPayloadV1{}, artifactInvalid(err)
	}
	if cloned.SchemaVersion != ArtifactSchemaVersion {
		return ArtifactPayloadV1{}, artifactInvalid(ErrFrozenSchemaVersion)
	}
	if cloned.TriggerConfig, err = canonicalRequiredArtifactJSONObject(
		cloned.TriggerConfig,
		"trigger_config",
	); err != nil {
		return ArtifactPayloadV1{}, artifactInvalid(err)
	}
	if cloned.GraphDefinition, err = canonicalRequiredArtifactJSONObject(
		cloned.GraphDefinition,
		"graph_definition",
	); err != nil {
		return ArtifactPayloadV1{}, artifactInvalid(err)
	}
	if cloned.Team.LeadAgentVersion == 0 && cloned.Team.LeadAgentContentHash != "" ||
		cloned.Team.LeadAgentVersion != 0 && !validLowerHexHash(cloned.Team.LeadAgentContentHash) {
		return ArtifactPayloadV1{}, artifactInvalid(ErrFrozenSchemaMismatch)
	}
	if cloned.Team.LeadAgentVersion != 0 {
		if err := validateJCSSafeInteger(cloned.Team.LeadAgentVersion); err != nil {
			return ArtifactPayloadV1{}, artifactInvalid(err)
		}
	}

	if cloned.Team.Workers == nil {
		cloned.Team.Workers = []FrozenTeamWorker{}
	}
	for index := range cloned.Team.Workers {
		if cloned.Team.Workers[index], err = normalizeFrozenTeamWorker(
			cloned.Team.Workers[index],
		); err != nil {
			return ArtifactPayloadV1{}, artifactInvalid(err)
		}
	}
	sort.Slice(cloned.Team.Workers, func(i, j int) bool {
		return compareUTF16Strings(
			cloned.Team.Workers[i].WorkerAgentID,
			cloned.Team.Workers[j].WorkerAgentID,
		) < 0
	})
	for index := 1; index < len(cloned.Team.Workers); index++ {
		if cloned.Team.Workers[index-1].WorkerAgentID ==
			cloned.Team.Workers[index].WorkerAgentID {
			return ArtifactPayloadV1{}, artifactInvalid(ErrFrozenDuplicateSetValue)
		}
	}

	if cloned.Bundles == nil {
		cloned.Bundles = []FrozenExecutionBundle{}
	}
	for index := range cloned.Bundles {
		if cloned.Bundles[index], err = normalizeFrozenExecutionBundle(
			cloned.Bundles[index],
		); err != nil {
			return ArtifactPayloadV1{}, artifactInvalid(err)
		}
	}
	sort.Slice(cloned.Bundles, func(i, j int) bool {
		left := cloned.Bundles[i].Agent
		right := cloned.Bundles[j].Agent
		if comparison := compareUTF16Strings(left.AgentID, right.AgentID); comparison != 0 {
			return comparison < 0
		}
		return left.AgentVersion < right.AgentVersion
	})
	for index := 1; index < len(cloned.Bundles); index++ {
		left := cloned.Bundles[index-1].Agent
		right := cloned.Bundles[index].Agent
		if left.AgentID == right.AgentID && left.AgentVersion == right.AgentVersion {
			return ArtifactPayloadV1{}, artifactInvalid(ErrFrozenDuplicateSetValue)
		}
	}

	if cloned.DeliveryTargets == nil {
		cloned.DeliveryTargets = []FrozenDeliveryTarget{}
	}
	for index := range cloned.DeliveryTargets {
		if cloned.DeliveryTargets[index], err = normalizeFrozenDeliveryTarget(
			cloned.DeliveryTargets[index],
		); err != nil {
			return ArtifactPayloadV1{}, artifactInvalid(err)
		}
	}
	sort.Slice(cloned.DeliveryTargets, func(i, j int) bool {
		left := cloned.DeliveryTargets[i]
		right := cloned.DeliveryTargets[j]
		if comparison := compareUTF16Strings(left.TargetID, right.TargetID); comparison != 0 {
			return comparison < 0
		}
		return left.TargetRevision < right.TargetRevision
	})
	for index := 1; index < len(cloned.DeliveryTargets); index++ {
		left := cloned.DeliveryTargets[index-1]
		right := cloned.DeliveryTargets[index]
		if left.TargetID == right.TargetID && left.TargetRevision == right.TargetRevision {
			return ArtifactPayloadV1{}, artifactInvalid(ErrFrozenDuplicateSetValue)
		}
	}
	return cloned, nil
}

func normalizeArtifactEnvelopeHashInputV1(
	value ArtifactEnvelopeHashInputV1,
) (ArtifactEnvelopeHashInputV1, error) {
	if err := validateValueUTF8(value); err != nil {
		return ArtifactEnvelopeHashInputV1{}, artifactInvalid(err)
	}
	cloned, err := cloneTyped(value)
	if err != nil {
		return ArtifactEnvelopeHashInputV1{}, artifactInvalid(err)
	}
	if cloned.WorkspaceID == "" || cloned.WorkflowID == "" || cloned.WorkflowVersion < 1 {
		return ArtifactEnvelopeHashInputV1{}, artifactInvalid(
			errors.New("artifact envelope identity is invalid"),
		)
	}
	if err := validateJCSSafeInteger(int64(cloned.WorkflowVersion)); err != nil {
		return ArtifactEnvelopeHashInputV1{}, artifactInvalid(err)
	}
	if cloned.ArtifactSchemaVersion != ArtifactSchemaVersion ||
		cloned.CanonicalizationAlgorithm != ArtifactCanonicalizationAlgorithm ||
		cloned.CanonicalizationVersion != ArtifactCanonicalizationVersion ||
		cloned.HashAlgorithm != ArtifactHashAlgorithm {
		return ArtifactEnvelopeHashInputV1{}, artifactInvalid(
			errors.New("artifact envelope algorithm contract is invalid"),
		)
	}
	cloned.Payload, err = normalizeArtifactPayloadV1(cloned.Payload)
	if err != nil {
		return ArtifactEnvelopeHashInputV1{}, artifactInvalid(err)
	}
	return cloned, nil
}

func ComputeArtifactContentHash(input ArtifactEnvelopeHashInputV1) (string, error) {
	normalized, err := normalizeArtifactEnvelopeHashInputV1(input)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalizeTyped(normalized)
	if err != nil {
		return "", artifactInvalid(err)
	}
	return sha256Hex(canonical), nil
}

func ComputeResourceContentHash(resource FrozenSkillResource) (string, error) {
	content, err := decodedResourceContent(resource)
	if err != nil {
		return "", err
	}
	return sha256Hex(content), nil
}

func ValidateResourceContentHash(resource FrozenSkillResource) error {
	got, err := ComputeResourceContentHash(resource)
	if err != nil {
		return err
	}
	if resource.ContentHash != got {
		return ErrFrozenContentHashMismatch
	}
	return nil
}

func decodedResourceContent(resource FrozenSkillResource) ([]byte, error) {
	switch resource.Encoding {
	case "utf8":
		if !utf8.ValidString(resource.Content) {
			return nil, errors.New("frozen resource content is not valid UTF-8")
		}
		return []byte(resource.Content), nil
	case "base64":
		decoded, err := base64.StdEncoding.Strict().DecodeString(resource.Content)
		if err != nil {
			return nil, fmt.Errorf("decode frozen resource base64: %w", err)
		}
		return decoded, nil
	default:
		return nil, errors.New("unsupported frozen resource encoding")
	}
}

func validateFrozenSkillResource(resource FrozenSkillResource) error {
	if resource.Kind != "script" && resource.Kind != "reference" {
		return errors.New("invalid frozen resource kind")
	}
	if resource.RelativePath == "" || strings.HasPrefix(resource.RelativePath, "/") {
		return errors.New("frozen resource path must be relative")
	}
	return ValidateResourceContentHash(resource)
}

func BuildFrozenDependencyManifest(
	dependencies []FrozenDependencyRef,
) (string, FrozenDependencyManifest, error) {
	copied, err := cloneTyped(dependencies)
	if err != nil {
		return "", FrozenDependencyManifest{}, err
	}
	if copied == nil {
		copied = []FrozenDependencyRef{}
	}
	for _, dependency := range copied {
		if err := validateFrozenDependencyRef(dependency); err != nil {
			return "", FrozenDependencyManifest{}, err
		}
	}
	sort.Slice(copied, func(i, j int) bool {
		return CompareFrozenDependency(copied[i], copied[j]) < 0
	})
	for index := 1; index < len(copied); index++ {
		if compareEnumeratedDependency(
			copied[index-1].EnumeratedDependencyRef,
			copied[index].EnumeratedDependencyRef,
		) == 0 {
			return "", FrozenDependencyManifest{}, ErrFrozenDuplicateDependency
		}
	}
	hash, err := ComputeManifestHash(copied)
	if err != nil {
		return "", FrozenDependencyManifest{}, err
	}
	return hash, FrozenDependencyManifest{
		SchemaVersion: FrozenSchemaVersion,
		Dependencies:  copied,
		ManifestHash:  hash,
	}, nil
}

func ComputeManifestHash(dependencies []FrozenDependencyRef) (string, error) {
	canonical, err := canonicalizeTyped(dependencies)
	if err != nil {
		return "", err
	}
	return sha256Hex(canonical), nil
}

func ValidateManifest(manifest FrozenDependencyManifest) error {
	if err := requireSchemaVersion(manifest.SchemaVersion); err != nil {
		return err
	}
	dependencies := manifest.Dependencies
	if dependencies == nil {
		return ErrFrozenManifestOrder
	}
	for _, dependency := range dependencies {
		if err := validateFrozenDependencyRef(dependency); err != nil {
			return err
		}
	}
	for index := 1; index < len(dependencies); index++ {
		comparison := CompareFrozenDependency(dependencies[index-1], dependencies[index])
		if comparison > 0 {
			return ErrFrozenManifestOrder
		}
		if compareEnumeratedDependency(
			dependencies[index-1].EnumeratedDependencyRef,
			dependencies[index].EnumeratedDependencyRef,
		) == 0 {
			return ErrFrozenDuplicateDependency
		}
	}
	want, err := ComputeManifestHash(dependencies)
	if err != nil {
		return err
	}
	if manifest.ManifestHash != want {
		return ErrFrozenContentHashMismatch
	}
	return nil
}

func validateEnumeratedDependencyRef(reference EnumeratedDependencyRef) error {
	if reference.WorkspaceID == "" || reference.OwnerID == "" ||
		reference.DependencyType == "" || reference.DependencyKey == "" {
		return errors.New("frozen dependency identity is incomplete")
	}
	switch reference.OwnerType {
	case "workflow":
		if reference.OwnerAgentVersion != nil {
			return errors.New("workflow dependency owner_agent_version must be null")
		}
	case "agent":
		if reference.OwnerAgentVersion == nil {
			return errors.New("agent dependency owner_agent_version is required")
		}
	default:
		return errors.New("invalid frozen dependency owner_type")
	}
	if reference.OwnerAgentVersion != nil {
		if err := validateJCSSafeInteger(*reference.OwnerAgentVersion); err != nil {
			return err
		}
	}
	if reference.DependencyVersion != nil {
		if err := validateJCSSafeInteger(*reference.DependencyVersion); err != nil {
			return err
		}
	}
	return nil
}

func validateFrozenDependencyRef(reference FrozenDependencyRef) error {
	if err := validateEnumeratedDependencyRef(reference.EnumeratedDependencyRef); err != nil {
		return err
	}
	if !validLowerHexHash(reference.ContentHash) {
		return errors.New("frozen dependency content_hash must be lowercase SHA-256")
	}
	return nil
}

func canonicalizeTyped(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode frozen DTO: %w", err)
	}
	return canonicalizeStrictJSON(raw)
}

func canonicalNullableRawJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return json.RawMessage("null"), nil
	}
	canonical, err := canonicalizeStrictJSON(raw)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}

func canonicalRequiredJSONObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("frozen factory_input is required")
	}
	canonical, err := canonicalizeStrictJSON(raw)
	if err != nil {
		return nil, err
	}
	if len(canonical) == 0 || canonical[0] != '{' {
		return nil, errors.New("frozen factory_input must be an object")
	}
	return json.RawMessage(canonical), nil
}

func canonicalRequiredArtifactJSONObject(
	raw json.RawMessage,
	field string,
) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("artifact %s is required", field)
	}
	canonical, err := canonicalizeStrictJSON(raw)
	if err != nil {
		return nil, err
	}
	if len(canonical) == 0 || canonical[0] != '{' {
		return nil, fmt.Errorf("artifact %s must be an object", field)
	}
	return json.RawMessage(canonical), nil
}

func isArtifactPreorder(schema PreorderSchema) bool {
	return schema == PreorderArtifactPayloadV1 ||
		schema == PreorderArtifactEnvelopeHashInputV1
}

func canonicalStringSet(values []string) ([]string, error) {
	if values == nil {
		return []string{}, nil
	}
	copied := append([]string(nil), values...)
	seen := make(map[string]struct{}, len(copied))
	for _, value := range copied {
		if _, exists := seen[value]; exists {
			return nil, ErrFrozenDuplicateSetValue
		}
		seen[value] = struct{}{}
	}
	sort.Slice(copied, func(i, j int) bool {
		return compareUTF16Strings(copied[i], copied[j]) < 0
	})
	return copied, nil
}

func canonicalMemorySlots(values []FrozenMemorySlot) ([]FrozenMemorySlot, error) {
	if values == nil {
		return []FrozenMemorySlot{}, nil
	}
	copied := append([]FrozenMemorySlot(nil), values...)
	sort.Slice(copied, func(i, j int) bool {
		if comparison := compareUTF16Strings(copied[i].Key, copied[j].Key); comparison != 0 {
			return comparison < 0
		}
		if comparison := compareUTF16Strings(copied[i].Label, copied[j].Label); comparison != 0 {
			return comparison < 0
		}
		return compareUTF16Strings(copied[i].Description, copied[j].Description) < 0
	})
	for index := 1; index < len(copied); index++ {
		if copied[index-1].Key == copied[index].Key {
			return nil, ErrFrozenDuplicateSetValue
		}
	}
	return copied, nil
}

func normalizeOrderedStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return append([]string(nil), values...)
}

func compareUTF16Strings(left, right string) int {
	leftUnits := utf16.Encode([]rune(left))
	rightUnits := utf16.Encode([]rune(right))
	for index := 0; index < len(leftUnits) && index < len(rightUnits); index++ {
		if leftUnits[index] < rightUnits[index] {
			return -1
		}
		if leftUnits[index] > rightUnits[index] {
			return 1
		}
	}
	switch {
	case len(leftUnits) < len(rightUnits):
		return -1
	case len(leftUnits) > len(rightUnits):
		return 1
	default:
		return 0
	}
}

func compareResource(left, right FrozenSkillResource) int {
	for _, pair := range [][2]string{
		{left.Kind, right.Kind},
		{left.RelativePath, right.RelativePath},
		{left.MediaType, right.MediaType},
		{left.Encoding, right.Encoding},
		{left.ContentHash, right.ContentHash},
	} {
		if comparison := compareUTF16Strings(pair[0], pair[1]); comparison != 0 {
			return comparison
		}
	}
	return 0
}

func compareSkill(left, right FrozenSkill) int {
	for _, pair := range [][2]string{
		{left.WorkspaceID, right.WorkspaceID},
		{left.Name, right.Name},
		{left.SourceType, right.SourceType},
		{left.SkillID, right.SkillID},
	} {
		if comparison := compareUTF16Strings(pair[0], pair[1]); comparison != 0 {
			return comparison
		}
	}
	return compareNullableInt64(left.SkillVersion, right.SkillVersion)
}

func compareCredential(left, right CredentialReference) int {
	for _, pair := range [][2]string{
		{left.WorkspaceID, right.WorkspaceID},
		{string(left.Kind), string(right.Kind)},
		{left.ResourceID, right.ResourceID},
		{left.Slot, right.Slot},
	} {
		if comparison := compareUTF16Strings(pair[0], pair[1]); comparison != 0 {
			return comparison
		}
	}
	return compareNullableInt64(left.CredentialVersion, right.CredentialVersion)
}

func compareEnumeratedDependency(left, right EnumeratedDependencyRef) int {
	for _, pair := range [][2]string{
		{left.WorkspaceID, right.WorkspaceID},
		{left.OwnerType, right.OwnerType},
		{left.OwnerID, right.OwnerID},
	} {
		if comparison := compareUTF16Strings(pair[0], pair[1]); comparison != 0 {
			return comparison
		}
	}
	if comparison := compareNullableInt64(
		left.OwnerAgentVersion,
		right.OwnerAgentVersion,
	); comparison != 0 {
		return comparison
	}
	for _, pair := range [][2]string{
		{left.DependencyType, right.DependencyType},
		{left.DependencyKey, right.DependencyKey},
	} {
		if comparison := compareUTF16Strings(pair[0], pair[1]); comparison != 0 {
			return comparison
		}
	}
	return compareNullableInt64(left.DependencyVersion, right.DependencyVersion)
}

// CompareFrozenDependency orders dependencies first by enumerated identity and
// then by ContentHash as a tie-break. Strings are compared by UTF-16 code unit
// order as part of the deterministic hashing contract. It returns a negative
// value when left sorts before right, zero when equal, and a positive value
// when left sorts after right.
func CompareFrozenDependency(left, right FrozenDependencyRef) int {
	if comparison := compareEnumeratedDependency(
		left.EnumeratedDependencyRef,
		right.EnumeratedDependencyRef,
	); comparison != 0 {
		return comparison
	}
	return compareUTF16Strings(left.ContentHash, right.ContentHash)
}

func compareNullableInt64(left, right *int64) int {
	switch {
	case left == nil && right == nil:
		return 0
	case left == nil:
		return -1
	case right == nil:
		return 1
	case *left < *right:
		return -1
	case *left > *right:
		return 1
	default:
		return 0
	}
}

func hasDuplicateEnumeratedDependencies(values []EnumeratedDependencyRef) bool {
	for index := 1; index < len(values); index++ {
		if compareEnumeratedDependency(values[index-1], values[index]) == 0 {
			return true
		}
	}
	return false
}

func validateManagedHTTPURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" {
		return errors.New("managed endpoint must be absolute http(s) without userinfo, query, or fragment")
	}
	return nil
}

func validLowercaseHTTPToken(value string) bool {
	if value == "" || value != strings.ToLower(value) {
		return false
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			continue
		}
		if strings.ContainsRune("!#$%&'*+-.^_`|~", rune(char)) {
			continue
		}
		return false
	}
	return true
}

func requireSchemaVersion(version int) error {
	if version != FrozenSchemaVersion {
		return ErrFrozenSchemaVersion
	}
	return nil
}

func validLowerHexHash(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneTyped[T any](value T) (T, error) {
	var cloned T
	raw, err := json.Marshal(value)
	if err != nil {
		return cloned, fmt.Errorf("clone frozen DTO: %w", err)
	}
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return cloned, fmt.Errorf("clone frozen DTO: %w", err)
	}
	return cloned, nil
}

func validateValueUTF8(value any) error {
	return validateReflectUTF8(reflect.ValueOf(value))
}

func validateReflectUTF8(value reflect.Value) error {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		return validateReflectUTF8(value.Elem())
	}
	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return errors.New("frozen DTO contains invalid UTF-8")
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if err := validateReflectUTF8(value.Field(index)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			if !utf8.Valid(value.Bytes()) {
				return errors.New("frozen DTO contains invalid UTF-8 bytes")
			}
			return nil
		}
		for index := 0; index < value.Len(); index++ {
			if err := validateReflectUTF8(value.Index(index)); err != nil {
				return err
			}
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateReflectUTF8(iterator.Key()); err != nil {
				return err
			}
			if err := validateReflectUTF8(iterator.Value()); err != nil {
				return err
			}
		}
	}
	return nil
}

type frozenSkillHashInput struct {
	SchemaVersion int                   `json:"schema_version"`
	WorkspaceID   string                `json:"workspace_id"`
	Name          string                `json:"name"`
	SourceType    string                `json:"source_type"`
	SkillID       string                `json:"skill_id"`
	SkillVersion  *int64                `json:"skill_version"`
	Description   string                `json:"description"`
	Body          string                `json:"body"`
	AlwaysActive  bool                  `json:"always_active"`
	Resources     []FrozenSkillResource `json:"resources"`
}

type frozenMCPHashInput struct {
	SchemaVersion  int                 `json:"schema_version"`
	WorkspaceID    string              `json:"workspace_id"`
	ServerID       string              `json:"server_id"`
	ServerRevision int64               `json:"server_revision"`
	Transport      string              `json:"transport"`
	URL            string              `json:"url"`
	Command        string              `json:"command"`
	Args           []string            `json:"args"`
	Filter         []string            `json:"filter"`
	WriteTools     []string            `json:"write_tools"`
	AccessRef      CredentialReference `json:"access_ref"`
}

type frozenModelHashInput struct {
	SchemaVersion    int                 `json:"schema_version"`
	WorkspaceID      string              `json:"workspace_id"`
	ProviderID       string              `json:"provider_id"`
	ProviderRevision int64               `json:"provider_revision"`
	ModelID          string              `json:"model_id"`
	BaseURL          string              `json:"base_url"`
	JSONObjectMode   bool                `json:"json_object_mode"`
	CredentialRef    CredentialReference `json:"credential_ref"`
}

type frozenRuntimeHashInput struct {
	SchemaVersion   int                 `json:"schema_version"`
	WorkspaceID     string              `json:"workspace_id"`
	RuntimeID       string              `json:"runtime_id"`
	Engine          string              `json:"engine"`
	RuntimeRevision int64               `json:"runtime_revision"`
	AccessRef       CredentialReference `json:"access_ref"`
}

type frozenDeliveryHashInput struct {
	SchemaVersion      int                               `json:"schema_version"`
	WorkspaceID        string                            `json:"workspace_id"`
	TargetID           string                            `json:"target_id"`
	TargetRevision     int64                             `json:"target_revision"`
	Kind               string                            `json:"kind"`
	Transport          string                            `json:"transport"`
	URL                string                            `json:"url"`
	Method             string                            `json:"method"`
	ContentType        string                            `json:"content_type"`
	TimeoutSeconds     int64                             `json:"timeout_seconds"`
	CredentialBindings []FrozenDeliveryCredentialBinding `json:"credential_bindings"`
	AccessRef          CredentialReference               `json:"access_ref"`
}
