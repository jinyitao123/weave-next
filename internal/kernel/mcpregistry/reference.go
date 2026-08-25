package mcpregistry

import (
	"context"
	"errors"
	"reflect"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

var _ credentials.TxReferenceSource = (*Store)(nil)

func (s *Store) ValidateReferenceTx(
	ctx context.Context,
	tx pgx.Tx,
	ref frozen.CredentialReference,
) error {
	if err := validateMCPReferenceInput(s, tx, ref); err != nil {
		return err
	}

	var state mcpReferenceState
	err := tx.QueryRow(ctx, `
		SELECT enabled, revoked_at, deleted_at
		FROM weave_mcp_servers
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, ref.WorkspaceID, ref.ResourceID).Scan(
		&state.enabled, &state.revokedAt, &state.deletedAt,
	)
	if err != nil {
		return classifyMCPReferenceReadError(err)
	}
	return validateMCPReferenceState(state)
}

func (s *Store) ResolveMCPAccessTx(
	ctx context.Context,
	tx pgx.Tx,
	ref frozen.CredentialReference,
) (credentials.SecretMaterial, error) {
	if err := validateMCPReferenceInput(s, tx, ref); err != nil {
		return credentials.SecretMaterial{}, err
	}

	var headersCipher, envCipher string
	var state mcpReferenceState
	err := tx.QueryRow(ctx, `
		SELECT headers_cipher, env_cipher, enabled, revoked_at, deleted_at
		FROM weave_mcp_servers
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, ref.WorkspaceID, ref.ResourceID).Scan(
		&headersCipher, &envCipher,
		&state.enabled, &state.revokedAt, &state.deletedAt,
	)
	if err != nil {
		return credentials.SecretMaterial{}, classifyMCPReferenceReadError(err)
	}
	if err := validateMCPReferenceState(state); err != nil {
		return credentials.SecretMaterial{}, err
	}

	headersPlaintext, err := secret.Open(s.key, headersCipher)
	if err != nil {
		return credentials.SecretMaterial{}, newMCPReferenceError(nil)
	}
	var material credentials.SecretMaterial
	err = withMCPReferenceSecretMap(
		headersPlaintext,
		func(headers map[string][]byte) error {
			environmentPlaintext, err := secret.Open(s.key, envCipher)
			if err != nil {
				return err
			}
			return withMCPReferenceSecretMap(
				environmentPlaintext,
				func(environment map[string][]byte) error {
					material = credentials.NewSecretMaterial(
						nil,
						headers,
						environment,
					)
					return nil
				},
			)
		},
	)
	if err != nil {
		return credentials.SecretMaterial{}, newMCPReferenceError(nil)
	}
	return material, nil
}

type mcpReferenceState struct {
	enabled   bool
	revokedAt *time.Time
	deletedAt *time.Time
}

func validateMCPReferenceInput(
	store *Store,
	tx pgx.Tx,
	ref frozen.CredentialReference,
) error {
	if err := credentials.ValidateReferenceV1(ref); err != nil {
		if errors.Is(err, credentials.ErrCredentialVersionUnsupported) {
			return err
		}
		return newMCPReferenceError(nil)
	}
	if ref.Kind != frozen.CredentialMCPServerAccess || ref.Slot != "access" {
		return newMCPReferenceError(nil)
	}
	if store == nil || store.pool == nil || nilMCPReferenceInterface(tx) {
		return newMCPReferenceError(nil)
	}
	return nil
}

func classifyMCPReferenceReadError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return newMCPReferenceError(nil)
	}
	return newMCPReferenceError(err)
}

func validateMCPReferenceState(state mcpReferenceState) error {
	if !state.enabled || state.revokedAt != nil || state.deletedAt != nil {
		return newMCPReferenceError(nil)
	}
	return nil
}

func withMCPReferenceSecretMap(
	plaintext []byte,
	consume func(map[string][]byte) error,
) error {
	temporary := make(map[string][]byte)
	if err := parseMCPReferenceSecretMap(plaintext, temporary); err != nil {
		return err
	}
	defer clearMCPReferenceSecretMap(temporary)
	if consume == nil {
		return errMCPReferenceSecretConsumerUnavailable
	}
	return consume(temporary)
}

var (
	errMCPReferenceSecretMapInvalid          = errors.New("MCP access secret map is invalid")
	errMCPReferenceSecretConsumerUnavailable = errors.New("MCP access secret consumer is unavailable")
)

func parseMCPReferenceSecretMap(
	plaintext []byte,
	values map[string][]byte,
) (err error) {
	defer clearMCPReferenceBytes(plaintext)
	if values == nil || len(values) != 0 {
		return errMCPReferenceSecretMapInvalid
	}
	defer func() {
		if err != nil {
			clearMCPReferenceSecretMap(values)
		}
	}()

	parser := mcpReferenceSecretParser{
		input:  plaintext,
		values: values,
	}
	return parser.parse()
}

type mcpReferenceSecretParser struct {
	input  []byte
	offset int
	values map[string][]byte
}

func (p *mcpReferenceSecretParser) parse() error {
	p.skipWhitespace()
	if !p.consume('{') {
		return errMCPReferenceSecretMapInvalid
	}
	p.skipWhitespace()
	if p.consume('}') {
		p.skipWhitespace()
		if p.offset != len(p.input) {
			return errMCPReferenceSecretMapInvalid
		}
		return nil
	}

	for {
		keyBytes, err := p.parseString()
		if err != nil {
			return err
		}
		key := string(keyBytes)
		clearMCPReferenceBytes(keyBytes)
		if _, duplicate := p.values[key]; duplicate {
			return errMCPReferenceSecretMapInvalid
		}

		p.skipWhitespace()
		if !p.consume(':') {
			return errMCPReferenceSecretMapInvalid
		}
		p.skipWhitespace()
		value, err := p.parseString()
		if err != nil {
			return err
		}
		p.values[key] = value

		p.skipWhitespace()
		if p.consume('}') {
			break
		}
		if !p.consume(',') {
			return errMCPReferenceSecretMapInvalid
		}
		p.skipWhitespace()
	}

	p.skipWhitespace()
	if p.offset != len(p.input) {
		return errMCPReferenceSecretMapInvalid
	}
	return nil
}

func (p *mcpReferenceSecretParser) parseString() (decoded []byte, err error) {
	span, err := p.scanString()
	if err != nil {
		return nil, err
	}
	decoded = make([]byte, span.decodedLength)
	defer func() {
		if err != nil {
			clearMCPReferenceBytes(decoded)
			decoded = nil
		}
	}()
	if err = p.decodeString(span, decoded); err != nil {
		return decoded, err
	}
	p.offset = span.end
	return decoded, nil
}

type mcpReferenceStringSpan struct {
	contentStart  int
	end           int
	decodedLength int
}

func (p *mcpReferenceSecretParser) scanString() (
	mcpReferenceStringSpan,
	error,
) {
	if p.offset >= len(p.input) || p.input[p.offset] != '"' {
		return mcpReferenceStringSpan{}, errMCPReferenceSecretMapInvalid
	}
	span := mcpReferenceStringSpan{contentStart: p.offset + 1}
	cursor := span.contentStart
	for cursor < len(p.input) {
		current := p.input[cursor]
		switch {
		case current == '"':
			span.end = cursor + 1
			return span, nil
		case current == '\\':
			decoded, next, ok := mcpReferenceEscapedRune(p.input, cursor+1)
			if !ok ||
				!addMCPReferenceDecodedLength(
					&span.decodedLength,
					utf8.RuneLen(decoded),
					len(p.input),
				) {
				return mcpReferenceStringSpan{}, errMCPReferenceSecretMapInvalid
			}
			cursor = next
		case current < 0x20:
			return mcpReferenceStringSpan{}, errMCPReferenceSecretMapInvalid
		case current < utf8.RuneSelf:
			if !addMCPReferenceDecodedLength(
				&span.decodedLength,
				1,
				len(p.input),
			) {
				return mcpReferenceStringSpan{}, errMCPReferenceSecretMapInvalid
			}
			cursor++
		default:
			_, size := utf8.DecodeRune(p.input[cursor:])
			if size == 1 ||
				!addMCPReferenceDecodedLength(
					&span.decodedLength,
					size,
					len(p.input),
				) {
				return mcpReferenceStringSpan{}, errMCPReferenceSecretMapInvalid
			}
			cursor += size
		}
	}
	return mcpReferenceStringSpan{}, errMCPReferenceSecretMapInvalid
}

func (p *mcpReferenceSecretParser) decodeString(
	span mcpReferenceStringSpan,
	decoded []byte,
) error {
	cursor := span.contentStart
	writeOffset := 0
	contentEnd := span.end - 1
	for cursor < contentEnd {
		current := p.input[cursor]
		switch {
		case current == '\\':
			value, next, ok := mcpReferenceEscapedRune(p.input, cursor+1)
			if !ok {
				return errMCPReferenceSecretMapInvalid
			}
			size := utf8.RuneLen(value)
			if size < 1 || writeOffset > len(decoded)-size {
				return errMCPReferenceSecretMapInvalid
			}
			if written := utf8.EncodeRune(decoded[writeOffset:], value); written != size {
				return errMCPReferenceSecretMapInvalid
			}
			writeOffset += size
			cursor = next
		case current < utf8.RuneSelf:
			if writeOffset >= len(decoded) {
				return errMCPReferenceSecretMapInvalid
			}
			decoded[writeOffset] = current
			writeOffset++
			cursor++
		default:
			_, size := utf8.DecodeRune(p.input[cursor:contentEnd])
			if size == 1 ||
				cursor > contentEnd-size ||
				writeOffset > len(decoded)-size {
				return errMCPReferenceSecretMapInvalid
			}
			copy(
				decoded[writeOffset:writeOffset+size],
				p.input[cursor:cursor+size],
			)
			writeOffset += size
			cursor += size
		}
	}
	if cursor != contentEnd || writeOffset != len(decoded) {
		return errMCPReferenceSecretMapInvalid
	}
	return nil
}

func addMCPReferenceDecodedLength(total *int, amount, limit int) bool {
	if amount < 0 || *total < 0 || amount > limit || *total > limit-amount {
		return false
	}
	*total += amount
	return true
}

func mcpReferenceEscapedRune(
	input []byte,
	offset int,
) (rune, int, bool) {
	if offset >= len(input) {
		return 0, offset, false
	}
	escaped := input[offset]
	offset++
	switch escaped {
	case '"', '\\', '/':
		return rune(escaped), offset, true
	case 'b':
		return '\b', offset, true
	case 'f':
		return '\f', offset, true
	case 'n':
		return '\n', offset, true
	case 'r':
		return '\r', offset, true
	case 't':
		return '\t', offset, true
	case 'u':
		first, next, ok := mcpReferenceHexCodeUnit(input, offset)
		if !ok {
			return 0, offset, false
		}
		codePoint := rune(first)
		switch {
		case first >= 0xd800 && first <= 0xdbff:
			if next > len(input)-2 ||
				input[next] != '\\' ||
				input[next+1] != 'u' {
				return 0, offset, false
			}
			second, afterSecond, ok := mcpReferenceHexCodeUnit(input, next+2)
			if !ok || second < 0xdc00 || second > 0xdfff {
				return 0, offset, false
			}
			codePoint = 0x10000 +
				(rune(first)-0xd800)<<10 +
				(rune(second) - 0xdc00)
			next = afterSecond
		case first >= 0xdc00 && first <= 0xdfff:
			return 0, offset, false
		}
		return codePoint, next, true
	default:
		return 0, offset, false
	}
}

func mcpReferenceHexCodeUnit(
	input []byte,
	offset int,
) (uint16, int, bool) {
	if offset > len(input)-4 {
		return 0, offset, false
	}
	var value uint16
	for index := 0; index < 4; index++ {
		digit, ok := mcpReferenceHexDigit(input[offset+index])
		if !ok {
			return 0, offset, false
		}
		value = value<<4 | uint16(digit)
	}
	return value, offset + 4, true
}

func mcpReferenceHexDigit(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func (p *mcpReferenceSecretParser) skipWhitespace() {
	for p.offset < len(p.input) {
		switch p.input[p.offset] {
		case ' ', '\t', '\n', '\r':
			p.offset++
		default:
			return
		}
	}
}

func (p *mcpReferenceSecretParser) consume(want byte) bool {
	if p.offset >= len(p.input) || p.input[p.offset] != want {
		return false
	}
	p.offset++
	return true
}

func clearMCPReferenceSecretMap(values map[string][]byte) {
	for _, value := range values {
		clearMCPReferenceBytes(value)
	}
}

func clearMCPReferenceBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func nilMCPReferenceInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type mcpReferenceError struct {
	cause error
}

func newMCPReferenceError(cause error) error {
	return &mcpReferenceError{cause: cause}
}

func (e *mcpReferenceError) Error() string {
	return credentials.CodeCredentialUnavailable
}

func (e *mcpReferenceError) Code() string {
	return credentials.CodeCredentialUnavailable
}

func (e *mcpReferenceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *mcpReferenceError) Is(target error) bool {
	if e == nil {
		return false
	}
	coded, ok := target.(credentials.CodedError)
	return ok &&
		!nilMCPReferenceInterface(coded) &&
		coded.Code() == credentials.CodeCredentialUnavailable
}
