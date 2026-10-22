package frozen

import (
	"crypto/sha256"
	"reflect"
	"sync"
)

// verifiedArtifacts memoizes DecodeArtifactEnvelopeV1. A published envelope
// is immutable and its decode is a pure function of the envelope, yet every
// invocation, fan-out leg, runtime load and credential check decodes the same
// one again; under load that strict decode, canonicalization and content hash
// took over a third of Weave's CPU. Entries are keyed by every envelope field,
// the payload by its SHA-256, and only successful decodes are kept. Each hit
// returns a deep copy, so no caller can alter what another one receives.
var verifiedArtifacts = artifactCache{limit: 512}

type artifactCacheKey struct {
	workspaceID, workflowID        string
	workflowVersion, schemaVersion int
	canonicalizationAlgorithm      string
	canonicalizationVersion        int
	hashAlgorithm, contentHash     string
	payload                        [sha256.Size]byte
}

func newArtifactCacheKey(envelope ArtifactEnvelopeV1) artifactCacheKey {
	return artifactCacheKey{
		workspaceID: envelope.WorkspaceID, workflowID: envelope.WorkflowID,
		workflowVersion: envelope.WorkflowVersion, schemaVersion: envelope.ArtifactSchemaVersion,
		canonicalizationAlgorithm: envelope.CanonicalizationAlgorithm,
		canonicalizationVersion:   envelope.CanonicalizationVersion,
		hashAlgorithm:             envelope.HashAlgorithm, contentHash: envelope.ContentHash,
		payload: sha256.Sum256(envelope.Payload),
	}
}

type artifactCache struct {
	mu      sync.Mutex
	limit   int
	entries map[artifactCacheKey]ArtifactPayloadV1
}

func (c *artifactCache) get(key artifactCacheKey) (ArtifactPayloadV1, bool) {
	c.mu.Lock()
	payload, ok := c.entries[key]
	c.mu.Unlock()
	if !ok {
		return ArtifactPayloadV1{}, false
	}
	return cloneArtifactPayload(payload), true
}

// put keeps its own copy of payload. Published versions are few, so a full
// cache simply starts over rather than tracking recency.
func (c *artifactCache) put(key artifactCacheKey, payload ArtifactPayloadV1) {
	payload = cloneArtifactPayload(payload)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= c.limit {
		c.entries = make(map[artifactCacheKey]ArtifactPayloadV1)
	}
	c.entries[key] = payload
}

func cloneArtifactPayload(payload ArtifactPayloadV1) ArtifactPayloadV1 {
	var out ArtifactPayloadV1
	deepCopyValue(reflect.ValueOf(&out).Elem(), reflect.ValueOf(payload))
	return out
}

// deepCopyValue copies src into the settable dst, sharing no slice, map or
// pointer. Nil stays nil and empty stays empty, so the copy encodes exactly
// as the original. The artifact types hold only exported data fields, which
// TestArtifactPayloadTypesAreDeepCopyable keeps true.
func deepCopyValue(dst, src reflect.Value) {
	switch src.Kind() {
	case reflect.Struct:
		for i := range src.NumField() {
			deepCopyValue(dst.Field(i), src.Field(i))
		}
	case reflect.Array:
		for i := range src.Len() {
			deepCopyValue(dst.Index(i), src.Index(i))
		}
	case reflect.Slice:
		if src.IsNil() {
			return
		}
		out := reflect.MakeSlice(src.Type(), src.Len(), src.Len())
		for i := range src.Len() {
			deepCopyValue(out.Index(i), src.Index(i))
		}
		dst.Set(out)
	case reflect.Map:
		if src.IsNil() {
			return
		}
		out := reflect.MakeMapWithSize(src.Type(), src.Len())
		for iter := src.MapRange(); iter.Next(); {
			value := reflect.New(src.Type().Elem()).Elem()
			deepCopyValue(value, iter.Value())
			out.SetMapIndex(iter.Key(), value)
		}
		dst.Set(out)
	case reflect.Pointer:
		if src.IsNil() {
			return
		}
		out := reflect.New(src.Type().Elem())
		deepCopyValue(out.Elem(), src.Elem())
		dst.Set(out)
	case reflect.Interface:
		if src.IsNil() {
			return
		}
		value := reflect.New(src.Elem().Type()).Elem()
		deepCopyValue(value, src.Elem())
		dst.Set(value)
	default:
		dst.Set(src)
	}
}
