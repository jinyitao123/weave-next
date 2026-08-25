package frozen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lattice-substrate/json-canon/jcs"
	"github.com/lattice-substrate/json-canon/jcserr"
)

// CanonicalizeJSON applies the repository's strict RFC 8785 adapter to a raw
// JSON value. Callers remain responsible for constructing typed, allowlisted
// DTOs before marshaling them into this boundary.
func CanonicalizeJSON(input []byte) ([]byte, error) {
	return canonicalizeStrictJSON(input)
}

// HashCanonicalJSON returns the lowercase SHA-256 digest of strict RFC 8785
// canonical JSON.
func HashCanonicalJSON(input []byte) (string, error) {
	canonical, err := CanonicalizeJSON(input)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

// CanonicalizePreordered applies the frozen rfc8785+jcs-preorder adapter after
// the caller has sorted every ABI-defined set-like array by its stable key.
func CanonicalizePreordered(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal preordered canonical value: %w", err)
	}
	return CanonicalizeJSON(encoded)
}

const MaxJCSSafeInteger int64 = 9007199254740991

var (
	ErrJCSInvalidInput     = errors.New("strict JCS input rejected")
	ErrJCSBoundExceeded    = errors.New("strict JCS resource bound exceeded")
	ErrJCSInternal         = errors.New("strict JCS internal failure")
	ErrJCSSafeIntegerRange = errors.New("JCS integer must be between 1 and 9007199254740991")
)

type JCSFailureClass string

const (
	JCSFailureInvalidUTF8        JCSFailureClass = "invalid_utf8"
	JCSFailureInvalidGrammar     JCSFailureClass = "invalid_grammar"
	JCSFailureDuplicateKey       JCSFailureClass = "duplicate_key"
	JCSFailureLoneSurrogate      JCSFailureClass = "lone_surrogate"
	JCSFailureNoncharacter       JCSFailureClass = "noncharacter"
	JCSFailureNumberOverflow     JCSFailureClass = "number_overflow"
	JCSFailureNumberNegativeZero JCSFailureClass = "number_negative_zero"
	JCSFailureNumberUnderflow    JCSFailureClass = "number_underflow"
	JCSFailureBoundExceeded      JCSFailureClass = "bound_exceeded"
	JCSFailureNotCanonical       JCSFailureClass = "not_canonical"
	JCSFailureInternal           JCSFailureClass = "internal"
)

// JCSError exposes a stable package-local failure class without retaining the
// upstream error or the rejected input.
type JCSError struct {
	Class JCSFailureClass
}

func (e *JCSError) Error() string {
	if e == nil {
		return ErrJCSInternal.Error()
	}
	return "strict JCS canonicalization failed: " + string(e.Class)
}

func (e *JCSError) Is(target error) bool {
	if e == nil {
		return target == ErrJCSInternal
	}
	switch target {
	case ErrJCSInvalidInput:
		return e.Class != JCSFailureBoundExceeded && e.Class != JCSFailureInternal
	case ErrJCSBoundExceeded:
		return e.Class == JCSFailureBoundExceeded
	case ErrJCSInternal:
		return e.Class == JCSFailureInternal
	default:
		return false
	}
}

func canonicalizeStrictJSON(input []byte) ([]byte, error) {
	canonical, err := jcs.Canonicalize(input)
	if err != nil {
		return nil, classifyJCSError(err)
	}
	return append([]byte(nil), canonical...), nil
}

func classifyJCSError(err error) error {
	var upstream *jcserr.Error
	if !errors.As(err, &upstream) {
		return &JCSError{Class: JCSFailureInternal}
	}

	class := JCSFailureInternal
	switch upstream.Class {
	case jcserr.InvalidUTF8:
		class = JCSFailureInvalidUTF8
	case jcserr.InvalidGrammar:
		class = JCSFailureInvalidGrammar
	case jcserr.DuplicateKey:
		class = JCSFailureDuplicateKey
	case jcserr.LoneSurrogate:
		class = JCSFailureLoneSurrogate
	case jcserr.Noncharacter:
		class = JCSFailureNoncharacter
	case jcserr.NumberOverflow:
		class = JCSFailureNumberOverflow
	case jcserr.NumberNegZero:
		class = JCSFailureNumberNegativeZero
	case jcserr.NumberUnderflow:
		class = JCSFailureNumberUnderflow
	case jcserr.BoundExceeded:
		class = JCSFailureBoundExceeded
	case jcserr.NotCanonical:
		class = JCSFailureNotCanonical
	case jcserr.CLIUsage, jcserr.InternalIO, jcserr.InternalError:
		class = JCSFailureInternal
	}
	return &JCSError{Class: class}
}

func validateJCSSafeInteger(value int64) error {
	return ValidateJCSSafeIntegerRange(value, 1, MaxJCSSafeInteger)
}

// ValidateJCSSafeIntegerRange validates an ABI-specific inclusive integer
// domain while enforcing the global JCS safe-integer ceiling.
func ValidateJCSSafeIntegerRange(value, min, max int64) error {
	if min < 0 || max < min || max > MaxJCSSafeInteger || value < min || value > max {
		return fmt.Errorf("%w: value %d outside [%d,%d]", ErrJCSSafeIntegerRange, value, min, max)
	}
	return nil
}
