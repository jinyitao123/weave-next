// Package deliverycheck evaluates published, bounded assertions over frozen data.
// It has no model, tool, network, filesystem, execution or publication authority.
package deliverycheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const ID = "weave.deterministic"
const Version = "v1"
const maxBytes = 1 << 20

// Selector reads a JSON value. Reductions operate on the selected array; Field
// selects a field of each item before sum/values. Literal never reads live state.
type Selector struct {
	Source   string          `json:"source"`
	Path     string          `json:"path,omitempty"`
	Artifact string          `json:"artifact,omitempty"`
	Reduce   string          `json:"reduce,omitempty"`
	Field    string          `json:"field,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
}

type Rule struct {
	Actual   Selector        `json:"actual"`
	Operator string          `json:"operator"`
	Expected *Selector       `json:"expected,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
}

// InputReader must resolve the original, immutable input of this exact run.
// It may not read a resume message, mutable conversation or model interpretation.
type InputReader func(context.Context, deliverable.Candidate) (json.RawMessage, error)

func ValidateContract(contract *deliverable.DeliveryContract) error {
	if err := deliverable.ValidateDeliveryContract(contract); err != nil {
		return err
	}
	if contract == nil {
		return nil
	}
	for _, check := range contract.RequiredChecks {
		if check.VerifierID != ID {
			continue
		}
		if strings.TrimSpace(check.Title) == "" {
			return fmt.Errorf("check %s: a user-readable requirement title is required", check.ID)
		}
		if check.VerifierVersion != Version {
			return fmt.Errorf("check %s: unsupported deterministic verifier version", check.ID)
		}
		if _, _, err := compile(check.Parameters); err != nil {
			return fmt.Errorf("check %s: %w", check.ID, err)
		}
	}
	return nil
}

func compile(raw json.RawMessage) (Rule, *jsonschema.Schema, error) {
	var rule Rule
	if _, err := parse(raw); err != nil {
		return rule, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rule); err != nil {
		return rule, nil, err
	}
	if rule.Actual.Source != "output" && rule.Actual.Source != "artifact" {
		return rule, nil, errors.New("actual must select final output or a final artifact")
	}
	if err := validateSelector(rule.Actual); err != nil {
		return rule, nil, err
	}
	switch rule.Operator {
	case "equals", "less_than", "at_most", "greater_than", "at_least", "set_equals":
		if rule.Expected == nil || len(rule.Schema) != 0 {
			return rule, nil, errors.New("comparison requires expected and forbids schema")
		}
		if err := validateSelector(*rule.Expected); err != nil {
			return rule, nil, err
		}
		if rule.Expected.Source == "literal" {
			value, err := selectValue(*rule.Expected, deliverable.Candidate{}, nil)
			if err != nil {
				return rule, nil, fmt.Errorf("invalid expected constant: %w", err)
			}
			switch rule.Operator {
			case "less_than", "at_most", "greater_than", "at_least":
				if _, ok := number(value); !ok {
					return rule, nil, errors.New("numeric comparison requires numeric expected constant")
				}
			case "set_equals":
				if _, ok := value.([]any); !ok {
					return rule, nil, errors.New("set comparison requires an array expected constant")
				}
			}
		}
	case "unique":
		if rule.Expected != nil || len(rule.Schema) != 0 {
			return rule, nil, errors.New("unique forbids expected and schema")
		}
	case "schema":
		if rule.Expected != nil || len(rule.Schema) == 0 {
			return rule, nil, errors.New("schema requires schema and forbids expected")
		}
		value, err := parse(rule.Schema)
		if err != nil {
			return rule, nil, err
		}
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(nil)
		const location = "https://weave.invalid/delivery-schema"
		if err := c.AddResource(location, value); err != nil {
			return rule, nil, err
		}
		schema, err := c.Compile(location)
		return rule, schema, err
	default:
		return rule, nil, errors.New("unsupported deterministic operator")
	}
	return rule, nil, nil
}

func validateSelector(s Selector) error {
	if !validPointer(s.Path) || !validPointer(s.Field) {
		return errors.New("invalid JSON pointer")
	}
	switch s.Source {
	case "input", "output":
		if s.Artifact != "" || len(s.Value) != 0 {
			return errors.New("data selector forbids artifact and value")
		}
	case "artifact":
		if s.Artifact == "" || len(s.Artifact) > 512 || len(s.Value) != 0 {
			return errors.New("artifact selector requires exact final artifact path")
		}
	case "literal":
		if s.Artifact != "" {
			return errors.New("literal forbids artifact")
		}
		if _, err := parse(s.Value); err != nil {
			return err
		}
	default:
		return errors.New("unsupported selector source")
	}
	switch s.Reduce {
	case "", "count":
		if s.Field != "" {
			return errors.New("field requires sum or values")
		}
	case "sum", "values":
	default:
		return errors.New("unsupported reduction")
	}
	return nil
}

// Verifier compares real final bytes. A model's self-rating is never consumed.
func Verifier(read InputReader) deliverable.Verifier {
	return func(ctx context.Context, input deliverable.VerificationInput) (deliverable.CheckResult, error) {
		evidence := map[string]any{"output_digest": input.Candidate.OutputDigest}
		finish := func(status deliverable.VerificationStatus, reason string) (deliverable.CheckResult, error) {
			raw, err := json.Marshal(evidence)
			return deliverable.CheckResult{Status: status, Reason: reason, Evidence: raw}, err
		}
		rule, schema, err := compile(input.Check.Parameters)
		if err != nil {
			return finish(deliverable.VerificationUnknown, "deterministic_rule_invalid")
		}
		evidence["operator"] = rule.Operator
		var frozenInput json.RawMessage
		if rule.Expected != nil && rule.Expected.Source == "input" {
			if read == nil {
				return finish(deliverable.VerificationUnknown, "deterministic_input_unavailable")
			}
			frozenInput, err = read(ctx, input.Candidate)
			if err != nil || len(frozenInput) > maxBytes {
				return finish(deliverable.VerificationUnknown, "deterministic_input_unavailable")
			}
			digest, digestErr := deliverable.CanonicalJSONDigest(frozenInput)
			if digestErr != nil {
				return finish(deliverable.VerificationUnknown, "deterministic_input_unavailable")
			}
			evidence["input_digest"] = digest
		}
		actual, err := selectValue(rule.Actual, input.Candidate, frozenInput)
		if err != nil {
			evidence["problem"] = err.Error()
			if err.Error() == "data_limit" || err.Error() == "artifact_collection_limited" {
				return finish(deliverable.VerificationUnknown, "verifier_observation_unavailable")
			}
			return finish(deliverable.VerificationFailed, "deterministic_output_invalid")
		}
		evidence["actual"] = preview(actual)
		var expected any
		if rule.Expected != nil {
			expected, err = selectValue(*rule.Expected, input.Candidate, frozenInput)
			if err != nil {
				evidence["problem"] = err.Error()
				return finish(deliverable.VerificationUnknown, "deterministic_expected_unavailable")
			}
			evidence["expected"] = preview(expected)
		}
		if err := ctx.Err(); err != nil {
			return finish(deliverable.VerificationUnknown, "verifier_observation_unavailable")
		}
		passed := false
		switch rule.Operator {
		case "equals":
			passed = key(actual) == key(expected)
		case "less_than", "at_most", "greater_than", "at_least":
			a, aok := number(actual)
			b, bok := number(expected)
			if !aok {
				return finish(deliverable.VerificationFailed, "deterministic_number_required")
			}
			if !bok {
				return finish(deliverable.VerificationUnknown, "deterministic_expected_unavailable")
			}
			cmp := a.Cmp(b)
			passed = rule.Operator == "less_than" && cmp < 0 || rule.Operator == "at_most" && cmp <= 0 || rule.Operator == "greater_than" && cmp > 0 || rule.Operator == "at_least" && cmp >= 0
		case "set_equals":
			a, aok := set(actual)
			b, bok := set(expected)
			if !aok {
				return finish(deliverable.VerificationFailed, "deterministic_array_required")
			}
			if !bok {
				return finish(deliverable.VerificationUnknown, "deterministic_expected_unavailable")
			}
			passed = key(a) == key(b)
		case "unique":
			a, ok := actual.([]any)
			if !ok {
				return finish(deliverable.VerificationFailed, "deterministic_array_required")
			}
			values, _ := set(actual)
			passed = len(a) == len(values)
		case "schema":
			passed = schema.Validate(actual) == nil
		}
		if passed {
			return finish(deliverable.VerificationPassed, "deterministic_satisfied")
		}
		return finish(deliverable.VerificationFailed, "deterministic_mismatch")
	}
}

func selectValue(s Selector, c deliverable.Candidate, input json.RawMessage) (any, error) {
	var raw json.RawMessage
	switch s.Source {
	case "output":
		raw = c.Output
	case "input":
		raw = input
	case "literal":
		raw = s.Value
	case "artifact":
		found := false
		for _, a := range c.Artifacts {
			if a.Path == strings.TrimPrefix(s.Artifact, "outputs/") {
				if found {
					return nil, errors.New("artifact_ambiguous")
				}
				found = true
				raw = []byte(a.Content)
			}
		}
		if !found {
			for _, obs := range c.SourceObservations {
				if obs.Collection == nil || obs.Collection.Complete {
					continue
				}
				for _, issue := range obs.Collection.Issues {
					if (issue.Kind == "limit" || issue.Kind == "error") && (issue.Path == "" || strings.TrimPrefix(issue.Path, "outputs/") == strings.TrimPrefix(s.Artifact, "outputs/")) {
						return nil, errors.New("artifact_collection_limited")
					}
				}
			}
			return nil, errors.New("artifact_missing")
		}
	}
	if len(raw) > maxBytes {
		return nil, errors.New("data_limit")
	}
	value, err := parse(raw)
	if err != nil {
		return nil, errors.New("structured_data_required")
	}
	value, err = pointer(value, s.Path)
	if err != nil {
		return nil, err
	}
	if s.Reduce == "" {
		return value, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("array_required")
	}
	if s.Reduce == "count" {
		return json.Number(strconv.Itoa(len(items))), nil
	}
	selected := make([]any, 0, len(items))
	sum := new(big.Rat)
	for _, item := range items {
		field, err := pointer(item, s.Field)
		if err != nil {
			return nil, err
		}
		selected = append(selected, field)
		if s.Reduce == "sum" {
			n, ok := number(field)
			if !ok {
				return nil, errors.New("numeric_field_required")
			}
			sum.Add(sum, n)
		}
	}
	if s.Reduce == "sum" {
		return json.Number(decimal(sum)), nil
	}
	return selected, nil
}

func validPointer(path string) bool {
	if len(path) > 1024 || (path != "" && !strings.HasPrefix(path, "/")) {
		return false
	}
	for i := 0; i < len(path); i++ {
		if path[i] == '~' {
			i++
			if i == len(path) || (path[i] != '0' && path[i] != '1') {
				return false
			}
		}
	}
	return true
}
func pointer(value any, path string) (any, error) {
	if path == "" {
		return value, nil
	}
	for _, part := range strings.Split(path[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[part]
			if !ok {
				return nil, errors.New("field_missing")
			}
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(v) || strconv.Itoa(i) != part {
				return nil, errors.New("index_missing")
			}
			value = v[i]
		default:
			return nil, errors.New("field_parent_invalid")
		}
	}
	return value, nil
}

func number(v any) (*big.Rat, bool) {
	switch n := v.(type) {
	case *big.Rat:
		return n, true
	case json.Number:
		return new(big.Rat).SetString(string(n))
	}
	return nil, false
}
func key(v any) string {
	if n, ok := number(v); ok {
		return "n:" + n.RatString()
	}
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := []string{}
		for _, k := range keys {
			parts = append(parts, strconv.Quote(k)+":"+key(x[k]))
		}
		return "o:{" + strings.Join(parts, ",") + "}"
	case []any:
		parts := []string{}
		for _, a := range x {
			parts = append(parts, key(a))
		}
		return "a:[" + strings.Join(parts, ",") + "]"
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}
func set(v any) ([]any, bool) {
	a, ok := v.([]any)
	if !ok {
		return nil, false
	}
	keys := map[string]bool{}
	for _, x := range a {
		keys[key(x)] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	result := make([]any, 0, len(sorted))
	for _, k := range sorted {
		result = append(result, k)
	}
	return result, true
}
func decimal(n *big.Rat) string {
	denominator := new(big.Int).Set(n.Denom())
	digits := 0
	for _, factor := range []int64{2, 5} {
		count := 0
		divisor := big.NewInt(factor)
		for new(big.Int).Mod(denominator, divisor).Sign() == 0 {
			denominator.Div(denominator, divisor)
			count++
		}
		if count > digits {
			digits = count
		}
	}
	return n.FloatString(digits)
}
func preview(v any) string {
	var raw []byte
	if n, ok := number(v); ok {
		raw = []byte(decimal(n))
	} else {
		raw, _ = json.Marshal(v)
	}
	if len(raw) > 240 {
		return string(bytes.Runes(raw)[:min(120, len(bytes.Runes(raw)))]) + "…"
	}
	return string(raw)
}

// Token parsing rejects ambiguous duplicate fields and bounds traversal as well
// as bytes. Numbers retain their exact decimal representation.
func parse(raw []byte) (any, error) {
	if len(raw) == 0 || len(raw) > maxBytes {
		return nil, errors.New("JSON size limit")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	remaining := 20000
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		remaining--
		if depth > 64 || remaining < 0 {
			return nil, errors.New("JSON complexity limit")
		}
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{':
				m := map[string]any{}
				for d.More() {
					k, err := d.Token()
					if err != nil {
						return nil, err
					}
					name, ok := k.(string)
					if !ok {
						return nil, errors.New("invalid key")
					}
					if _, exists := m[name]; exists {
						return nil, errors.New("duplicate key")
					}
					v, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					m[name] = v
				}
				_, err = d.Token()
				return m, err
			case '[':
				a := []any{}
				for d.More() {
					v, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					a = append(a, v)
				}
				_, err = d.Token()
				return a, err
			}
			return nil, errors.New("unexpected delimiter")
		}
		if n, ok := token.(json.Number); ok {
			if len(n) > 128 {
				return nil, errors.New("number limit")
			}
			if i := strings.IndexAny(string(n), "eE"); i >= 0 {
				exponent, err := strconv.Atoi(string(n)[i+1:])
				if err != nil || exponent < -4096 || exponent > 4096 {
					return nil, errors.New("number exponent limit")
				}
			}
			f, ok := new(big.Rat).SetString(string(n))
			if !ok || f.Num().BitLen() > 16384 || f.Denom().BitLen() > 16384 {
				return nil, errors.New("number limit")
			}
		}
		return token, nil
	}
	v, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return v, nil
}
