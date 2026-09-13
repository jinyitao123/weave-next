package capability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type localSchemas struct{}

func (localSchemas) Load(string) (any, error) {
	return nil, fmt.Errorf("external schema references are unsupported")
}

func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(canonical, &value); err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(localSchemas{})
	const location = "urn:weave:capability:schema"
	if err := c.AddResource(location, value); err != nil {
		return nil, err
	}
	return c.Compile(location)
}

func ValidateValue(schema, value json.RawMessage) error {
	compiled, err := compileSchema(schema)
	if err != nil {
		return fmt.Errorf("invalid schema: %w", err)
	}
	canonical, err := frozen.CanonicalizeJSON(value)
	if err != nil {
		return err
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	return compiled.Validate(decoded)
}
