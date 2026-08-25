package delivery

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
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
	slot, headerName, err := validateDeliveryReferenceInput(s, tx, ref)
	if err != nil {
		return err
	}

	var state deliveryReferenceState
	if slot == deliveryReferenceAccessSlot {
		err = tx.QueryRow(ctx, `
			SELECT enabled, revoked_at, deleted_at
			FROM weave_delivery_targets
			WHERE workspace_id=$1 AND id=$2
			FOR SHARE
		`, ref.WorkspaceID, ref.ResourceID).Scan(
			&state.enabled, &state.revokedAt, &state.deletedAt,
		)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT h.enabled, h.revoked_at, h.deleted_at
			FROM weave_delivery_targets AS h
			JOIN weave_delivery_target_revisions AS r
			  ON r.workspace_id=h.workspace_id
			 AND r.target_id=h.id
			 AND r.revision=h.latest_revision
			WHERE h.workspace_id=$1 AND h.id=$2
			  AND $3=ANY(r.header_names)
			FOR SHARE OF h, r
		`, ref.WorkspaceID, ref.ResourceID, headerName).Scan(
			&state.enabled, &state.revokedAt, &state.deletedAt,
		)
	}
	if err != nil {
		return classifyDeliveryReferenceReadError(err)
	}
	return validateDeliveryReferenceState(state)
}

func (s *Store) ResolveDeliveryAccessTx(
	ctx context.Context,
	tx pgx.Tx,
	ref frozen.CredentialReference,
) (credentials.SecretMaterial, error) {
	slot, headerName, err := validateDeliveryReferenceInput(s, tx, ref)
	if err != nil {
		return credentials.SecretMaterial{}, err
	}
	if slot == deliveryReferenceAccessSlot {
		var state deliveryReferenceState
		err = tx.QueryRow(ctx, `
			SELECT enabled, revoked_at, deleted_at
			FROM weave_delivery_targets
			WHERE workspace_id=$1 AND id=$2
			FOR SHARE
		`, ref.WorkspaceID, ref.ResourceID).Scan(
			&state.enabled, &state.revokedAt, &state.deletedAt,
		)
		if err != nil {
			return credentials.SecretMaterial{}, classifyDeliveryReferenceReadError(err)
		}
		if err := validateDeliveryReferenceState(state); err != nil {
			return credentials.SecretMaterial{}, err
		}
		return credentials.SecretMaterial{}, nil
	}

	var (
		headersCipher string
		headerNames   []string
		state         deliveryReferenceState
	)
	err = tx.QueryRow(ctx, `
		SELECT h.headers_cipher, h.enabled, h.revoked_at, h.deleted_at,
		       r.header_names
		FROM weave_delivery_targets AS h
		JOIN weave_delivery_target_revisions AS r
		  ON r.workspace_id=h.workspace_id
		 AND r.target_id=h.id
		 AND r.revision=h.latest_revision
		WHERE h.workspace_id=$1 AND h.id=$2
		  AND $3=ANY(r.header_names)
		FOR SHARE OF h, r
	`, ref.WorkspaceID, ref.ResourceID, headerName).Scan(
		&headersCipher,
		&state.enabled, &state.revokedAt, &state.deletedAt,
		&headerNames,
	)
	if err != nil {
		return credentials.SecretMaterial{}, classifyDeliveryReferenceReadError(err)
	}
	if err := validateDeliveryReferenceState(state); err != nil {
		return credentials.SecretMaterial{}, err
	}
	if !validDeliveryReferenceHeaderNames(headerNames) {
		return credentials.SecretMaterial{}, newDeliveryReferenceError(nil)
	}

	opened, err := secret.Open(s.key, headersCipher)
	if err != nil {
		return credentials.SecretMaterial{}, newDeliveryReferenceError(nil)
	}
	plaintext := cloneDeliveryReferenceBytesExact(opened)
	clearDeliveryReferenceBytes(opened)

	var material credentials.SecretMaterial
	err = withDeliveryReferenceSecretMap(
		plaintext,
		headerNames,
		func(values map[string][]byte) error {
			if !deliveryReferenceKeysMatch(values, headerNames) {
				return errDeliveryReferenceSecretMapInvalid
			}
			value, ok := values[headerName]
			if !ok {
				return errDeliveryReferenceSecretMapInvalid
			}
			material = credentials.NewSecretMaterial(value, nil, nil)
			return nil
		},
	)
	if err != nil {
		return credentials.SecretMaterial{}, newDeliveryReferenceError(nil)
	}
	return material, nil
}

const (
	deliveryReferenceAccessSlot = "access"
	deliveryReferenceHeaderSlot = "header:"
)

type deliveryReferenceState struct {
	enabled   bool
	revokedAt *time.Time
	deletedAt *time.Time
}

func validateDeliveryReferenceInput(
	store *Store,
	tx pgx.Tx,
	ref frozen.CredentialReference,
) (string, string, error) {
	if err := credentials.ValidateReferenceV1(ref); err != nil {
		if errors.Is(err, credentials.ErrCredentialVersionUnsupported) {
			return "", "", newDeliveryReferenceVersionError(err)
		}
		return "", "", newDeliveryReferenceError(nil)
	}
	if ref.Kind != frozen.CredentialDeliveryTargetAccess {
		return "", "", newDeliveryReferenceError(nil)
	}

	slot := deliveryReferenceAccessSlot
	headerName := ""
	if ref.Slot != deliveryReferenceAccessSlot {
		if !strings.HasPrefix(ref.Slot, deliveryReferenceHeaderSlot) {
			return "", "", newDeliveryReferenceError(nil)
		}
		slot = deliveryReferenceHeaderSlot
		headerName = strings.TrimPrefix(ref.Slot, deliveryReferenceHeaderSlot)
		if !validDeliveryReferenceHeaderName(headerName) {
			return "", "", newDeliveryReferenceError(nil)
		}
	}
	if store == nil || store.pool == nil || nilDeliveryReferenceInterface(tx) {
		return "", "", newDeliveryReferenceError(nil)
	}
	return slot, headerName, nil
}

func classifyDeliveryReferenceReadError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return newDeliveryReferenceError(nil)
	}
	return newDeliveryReferenceError(err)
}

func validateDeliveryReferenceState(state deliveryReferenceState) error {
	if !state.enabled || state.revokedAt != nil || state.deletedAt != nil {
		return newDeliveryReferenceError(nil)
	}
	return nil
}

func validDeliveryReferenceHeaderName(name string) bool {
	return validASCIIHTTPToken(name) && lowercaseASCII(name) == name
}

func validDeliveryReferenceHeaderNames(names []string) bool {
	if names == nil {
		return false
	}
	for index, name := range names {
		if !validDeliveryReferenceHeaderName(name) {
			return false
		}
		if index > 0 && names[index-1] >= name {
			return false
		}
	}
	return true
}

func deliveryReferenceKeysMatch(
	values map[string][]byte,
	headerNames []string,
) bool {
	if len(values) != len(headerNames) {
		return false
	}
	for _, name := range headerNames {
		if _, ok := values[name]; !ok {
			return false
		}
	}
	return true
}

var (
	errDeliveryReferenceSecretMapInvalid = errors.New(
		"delivery access secret map is invalid",
	)
	errDeliveryReferenceSecretConsumerUnavailable = errors.New(
		"delivery access secret consumer is unavailable",
	)
)

func withDeliveryReferenceSecretMap(
	plaintext []byte,
	headerNames []string,
	consume func(map[string][]byte) error,
) error {
	values := make(map[string][]byte)
	if err := parseDeliveryReferenceSecretMap(
		plaintext,
		headerNames,
		values,
	); err != nil {
		return err
	}
	defer clearDeliveryReferenceSecretMap(values)
	if consume == nil {
		return errDeliveryReferenceSecretConsumerUnavailable
	}
	return consume(values)
}

func parseDeliveryReferenceSecretMap(
	plaintext []byte,
	headerNames []string,
	values map[string][]byte,
) (err error) {
	defer clearDeliveryReferenceBytes(plaintext)
	if values == nil || len(values) != 0 {
		return errDeliveryReferenceSecretMapInvalid
	}
	defer func() {
		if err != nil {
			clearDeliveryReferenceSecretMap(values)
		}
	}()

	parser := deliveryReferenceSecretParser{
		input:       plaintext,
		values:      values,
		headerNames: headerNames,
	}
	return parser.parse()
}

type deliveryReferenceSecretParser struct {
	input             []byte
	offset            int
	values            map[string][]byte
	headerNames       []string
	observeDecodedKey func([]byte)
}

func (p *deliveryReferenceSecretParser) parse() error {
	p.skipWhitespace()
	if !p.consume('{') {
		return errDeliveryReferenceSecretMapInvalid
	}
	p.skipWhitespace()
	if p.consume('}') {
		p.skipWhitespace()
		if p.offset != len(p.input) || len(p.headerNames) != 0 {
			return errDeliveryReferenceSecretMapInvalid
		}
		return nil
	}

	for {
		keyBytes, err := p.parseString()
		if err != nil {
			return err
		}
		if p.observeDecodedKey != nil {
			p.observeDecodedKey(keyBytes)
		}
		key, err := consumeDeliveryReferenceHeaderName(
			keyBytes,
			p.headerNames,
		)
		if err != nil {
			return err
		}
		if _, duplicate := p.values[key]; duplicate {
			return errDeliveryReferenceSecretMapInvalid
		}

		p.skipWhitespace()
		if !p.consume(':') {
			return errDeliveryReferenceSecretMapInvalid
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
			return errDeliveryReferenceSecretMapInvalid
		}
		p.skipWhitespace()
	}

	p.skipWhitespace()
	if p.offset != len(p.input) || len(p.values) != len(p.headerNames) {
		return errDeliveryReferenceSecretMapInvalid
	}
	return nil
}

func consumeDeliveryReferenceHeaderName(
	keyBytes []byte,
	headerNames []string,
) (string, error) {
	defer clearDeliveryReferenceBytes(keyBytes)
	for _, publicName := range headerNames {
		if bytes.Equal(keyBytes, []byte(publicName)) {
			return publicName, nil
		}
	}
	return "", errDeliveryReferenceSecretMapInvalid
}

func (p *deliveryReferenceSecretParser) parseString() (
	decoded []byte,
	err error,
) {
	span, err := p.scanString()
	if err != nil {
		return nil, err
	}
	decoded = make([]byte, span.decodedLength)
	defer func() {
		if err != nil {
			clearDeliveryReferenceBytes(decoded)
			decoded = nil
		}
	}()
	if err = p.decodeString(span, decoded); err != nil {
		return decoded, err
	}
	p.offset = span.end
	return decoded, nil
}

type deliveryReferenceStringSpan struct {
	contentStart  int
	end           int
	decodedLength int
}

func (p *deliveryReferenceSecretParser) scanString() (
	deliveryReferenceStringSpan,
	error,
) {
	if p.offset >= len(p.input) || p.input[p.offset] != '"' {
		return deliveryReferenceStringSpan{}, errDeliveryReferenceSecretMapInvalid
	}
	span := deliveryReferenceStringSpan{contentStart: p.offset + 1}
	cursor := span.contentStart
	for cursor < len(p.input) {
		current := p.input[cursor]
		switch {
		case current == '"':
			span.end = cursor + 1
			return span, nil
		case current == '\\':
			decoded, next, ok := deliveryReferenceEscapedRune(p.input, cursor+1)
			if !ok ||
				!addDeliveryReferenceDecodedLength(
					&span.decodedLength,
					utf8.RuneLen(decoded),
					len(p.input),
				) {
				return deliveryReferenceStringSpan{}, errDeliveryReferenceSecretMapInvalid
			}
			cursor = next
		case current < 0x20:
			return deliveryReferenceStringSpan{}, errDeliveryReferenceSecretMapInvalid
		case current < utf8.RuneSelf:
			if !addDeliveryReferenceDecodedLength(
				&span.decodedLength,
				1,
				len(p.input),
			) {
				return deliveryReferenceStringSpan{}, errDeliveryReferenceSecretMapInvalid
			}
			cursor++
		default:
			_, size := utf8.DecodeRune(p.input[cursor:])
			if size == 1 ||
				!addDeliveryReferenceDecodedLength(
					&span.decodedLength,
					size,
					len(p.input),
				) {
				return deliveryReferenceStringSpan{}, errDeliveryReferenceSecretMapInvalid
			}
			cursor += size
		}
	}
	return deliveryReferenceStringSpan{}, errDeliveryReferenceSecretMapInvalid
}

func (p *deliveryReferenceSecretParser) decodeString(
	span deliveryReferenceStringSpan,
	decoded []byte,
) error {
	cursor := span.contentStart
	writeOffset := 0
	contentEnd := span.end - 1
	for cursor < contentEnd {
		current := p.input[cursor]
		switch {
		case current == '\\':
			value, next, ok := deliveryReferenceEscapedRune(p.input, cursor+1)
			if !ok {
				return errDeliveryReferenceSecretMapInvalid
			}
			size := utf8.RuneLen(value)
			if size < 1 || writeOffset > len(decoded)-size {
				return errDeliveryReferenceSecretMapInvalid
			}
			if written := utf8.EncodeRune(decoded[writeOffset:], value); written != size {
				return errDeliveryReferenceSecretMapInvalid
			}
			writeOffset += size
			cursor = next
		case current < utf8.RuneSelf:
			if writeOffset >= len(decoded) {
				return errDeliveryReferenceSecretMapInvalid
			}
			decoded[writeOffset] = current
			writeOffset++
			cursor++
		default:
			_, size := utf8.DecodeRune(p.input[cursor:contentEnd])
			if size == 1 ||
				cursor > contentEnd-size ||
				writeOffset > len(decoded)-size {
				return errDeliveryReferenceSecretMapInvalid
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
		return errDeliveryReferenceSecretMapInvalid
	}
	return nil
}

func addDeliveryReferenceDecodedLength(total *int, amount, limit int) bool {
	if amount < 0 || *total < 0 || amount > limit || *total > limit-amount {
		return false
	}
	*total += amount
	return true
}

func deliveryReferenceEscapedRune(
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
		first, next, ok := deliveryReferenceHexCodeUnit(input, offset)
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
			second, afterSecond, ok := deliveryReferenceHexCodeUnit(input, next+2)
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

func deliveryReferenceHexCodeUnit(
	input []byte,
	offset int,
) (uint16, int, bool) {
	if offset > len(input)-4 {
		return 0, offset, false
	}
	var value uint16
	for index := 0; index < 4; index++ {
		digit, ok := deliveryReferenceHexDigit(input[offset+index])
		if !ok {
			return 0, offset, false
		}
		value = value<<4 | uint16(digit)
	}
	return value, offset + 4, true
}

func deliveryReferenceHexDigit(value byte) (byte, bool) {
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

func (p *deliveryReferenceSecretParser) skipWhitespace() {
	for p.offset < len(p.input) {
		switch p.input[p.offset] {
		case ' ', '\t', '\n', '\r':
			p.offset++
		default:
			return
		}
	}
}

func (p *deliveryReferenceSecretParser) consume(want byte) bool {
	if p.offset >= len(p.input) || p.input[p.offset] != want {
		return false
	}
	p.offset++
	return true
}

func cloneDeliveryReferenceBytesExact(value []byte) []byte {
	cloned := make([]byte, len(value))
	copy(cloned, value)
	return cloned
}

func clearDeliveryReferenceSecretMap(values map[string][]byte) {
	for _, value := range values {
		clearDeliveryReferenceBytes(value)
	}
}

func clearDeliveryReferenceBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func nilDeliveryReferenceInterface(value any) bool {
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

type deliveryReferenceError struct {
	code  string
	cause error
}

func newDeliveryReferenceError(cause error) error {
	return &deliveryReferenceError{
		code:  credentials.CodeCredentialUnavailable,
		cause: cause,
	}
}

func newDeliveryReferenceVersionError(cause error) error {
	return &deliveryReferenceError{
		code:  credentials.CodeCredentialVersionUnsupported,
		cause: cause,
	}
}

func (e *deliveryReferenceError) Error() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *deliveryReferenceError) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *deliveryReferenceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *deliveryReferenceError) Is(target error) bool {
	if e == nil {
		return false
	}
	coded, ok := target.(credentials.CodedError)
	return ok &&
		!nilDeliveryReferenceInterface(coded) &&
		coded.Code() == e.code
}
