package capability

// DefinitionSnapshot freezes developer content without allocating a published
// revision. Its identity is the canonical definition hash.
type DefinitionSnapshot struct {
	Definition     Definition `json:"definition"`
	DefinitionHash string     `json:"definition_hash"`
}

func FreezeDraft(d Definition) (DefinitionSnapshot, error) {
	// Publish is a pure canonicalization constructor, not a persistence action.
	frozen, err := Publish(d, 1)
	if err != nil {
		return DefinitionSnapshot{}, err
	}
	return DefinitionSnapshot{Definition: frozen.Definition, DefinitionHash: frozen.DefinitionHash}, nil
}

func CompileDebug(snapshot DefinitionSnapshot) (Plan, error) {
	plan, err := Compile(PublishedRevision{
		SchemaVersion: SchemaVersionV1, CapabilityID: snapshot.Definition.CapabilityID,
		Revision: 1, Definition: snapshot.Definition, DefinitionHash: snapshot.DefinitionHash,
	})
	if err != nil {
		return Plan{}, err
	}
	plan.Revision = 0
	return plan, nil
}
