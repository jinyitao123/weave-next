package runtimes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A device is a runtime one person enrolled for themselves with a one-time
// code (weave-workbench decision 009). The code is the only credential of the
// exchange: it names the workspace and the person, and works once.

const (
	DeviceEnrollmentTTL   = 10 * time.Minute
	enrollmentCodeLength  = 8
	enrollmentCodeLetters = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	deviceNameLimit       = 60
)

var (
	ErrEnrollmentCodeInvalid  = errors.New("enrollment code invalid")
	ErrEnrollmentCodeExpired  = errors.New("enrollment code expired")
	ErrEnrollmentCodeUsed     = errors.New("enrollment code used")
	ErrRuntimeBindingMismatch = errors.New("runtime belongs to another account")
)

// DeviceEnrollment is a freshly issued code, shown to its owner once.
type DeviceEnrollment struct {
	Code      string
	ExpiresAt time.Time
}

// EnrolledDevice is what the machine receives in exchange for a code.
type EnrolledDevice struct {
	RuntimeID string
	Token     string
	Name      string
}

// IssueDeviceEnrollment creates a one-time code for one person.
func (s *Store) IssueDeviceEnrollment(ctx context.Context, workspaceID, userID string) (DeviceEnrollment, error) {
	if workspaceID == "" || userID == "" {
		return DeviceEnrollment{}, errors.New("device enrollment needs a person")
	}
	code, err := newEnrollmentCode()
	if err != nil {
		return DeviceEnrollment{}, fmt.Errorf("generate enrollment code: %w", err)
	}
	expires := s.now().Add(DeviceEnrollmentTTL)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO weave_device_enrollments (id, workspace_id, user_id, code_digest, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, uuid.New(), workspaceID, userID, enrollmentDigest(code), s.now(), expires); err != nil {
		return DeviceEnrollment{}, fmt.Errorf("issue enrollment code: %w", err)
	}
	return DeviceEnrollment{Code: code[:4] + "-" + code[4:], ExpiresAt: expires}, nil
}

// EnrollDevice spends a code. With runtimeID naming a device this person
// already owns, the machine is re-enrolled under a new token and the old one
// stops working; otherwise a new device runtime is created. The code is spent
// in the same transaction, so a failure leaves it usable.
func (s *Store) EnrollDevice(ctx context.Context, code, hostname, runtimeID string) (EnrolledDevice, error) {
	digest := enrollmentDigest(normalizeEnrollmentCode(code))
	if digest == "" {
		return EnrolledDevice{}, ErrEnrollmentCodeInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return EnrolledDevice{}, fmt.Errorf("begin device enrollment: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var enrollmentID uuid.UUID
	var workspaceID, userID string
	var expiresAt time.Time
	var usedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT id, workspace_id, user_id, expires_at, used_at
		FROM weave_device_enrollments WHERE code_digest=$1 FOR UPDATE
	`, digest).Scan(&enrollmentID, &workspaceID, &userID, &expiresAt, &usedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnrolledDevice{}, ErrEnrollmentCodeInvalid
	}
	if err != nil {
		return EnrolledDevice{}, fmt.Errorf("read enrollment code: %w", err)
	}
	if usedAt != nil {
		return EnrolledDevice{}, ErrEnrollmentCodeUsed
	}
	now := s.now()
	if !now.Before(expiresAt) {
		return EnrolledDevice{}, ErrEnrollmentCodeExpired
	}

	token, err := generateToken()
	if err != nil {
		return EnrolledDevice{}, fmt.Errorf("generate runtime token: %w", err)
	}
	device := EnrolledDevice{Token: token}
	if runtimeID != "" {
		var owner *string
		err = tx.QueryRow(ctx, `
			SELECT device_owner_user_id, name FROM weave_runtimes
			WHERE workspace_id=$1 AND id=$2 AND deleted_at IS NULL AND revoked_at IS NULL FOR UPDATE
		`, workspaceID, runtimeID).Scan(&owner, &device.Name)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// The device this machine remembers is gone; enroll it afresh.
		case err != nil:
			return EnrolledDevice{}, fmt.Errorf("read enrolled device: %w", err)
		case owner == nil || *owner != userID:
			return EnrolledDevice{}, ErrRuntimeBindingMismatch
		default:
			if _, err := tx.Exec(ctx, `UPDATE weave_runtimes SET token_hash=$3, updated_at=$4 WHERE workspace_id=$1 AND id=$2`,
				workspaceID, runtimeID, hashToken(token), now); err != nil {
				return EnrolledDevice{}, fmt.Errorf("rotate device token: %w", err)
			}
			device.RuntimeID = runtimeID
		}
	}
	if device.RuntimeID == "" {
		name, err := freeRuntimeName(ctx, tx, workspaceID, deviceName(hostname))
		if err != nil {
			return EnrolledDevice{}, err
		}
		device.RuntimeID, device.Name = uuid.NewString(), name
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_runtimes (
				id, workspace_id, name, engines, token_hash,
				functional_revision, enabled, created_at, updated_at, device_owner_user_id
			) VALUES ($1, $2, $3, '[]', $4, 1, true, $5, $5, $6)
		`, device.RuntimeID, workspaceID, name, hashToken(token), now, userID); err != nil {
			return EnrolledDevice{}, fmt.Errorf("create device runtime: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_device_enrollments SET used_at=$2, runtime_id=$3 WHERE id=$1`,
		enrollmentID, now, device.RuntimeID); err != nil {
		return EnrolledDevice{}, fmt.Errorf("spend enrollment code: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return EnrolledDevice{}, fmt.Errorf("commit device enrollment: %w", err)
	}
	return device, nil
}

// freeRuntimeName returns base, or base with the first free " (n)" suffix;
// runtime names are unique within a workspace, deleted runtimes included.
func freeRuntimeName(ctx context.Context, tx pgx.Tx, workspaceID, base string) (string, error) {
	rows, err := tx.Query(ctx, `SELECT name FROM weave_runtimes WHERE workspace_id=$1 AND left(name, length($2))=$2`, workspaceID, base)
	if err != nil {
		return "", fmt.Errorf("read runtime names: %w", err)
	}
	taken := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", fmt.Errorf("read runtime names: %w", err)
		}
		taken[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read runtime names: %w", err)
	}
	name := base
	for n := 2; taken[name]; n++ {
		name = fmt.Sprintf("%s (%d)", base, n)
	}
	return name, nil
}

// deviceName keeps the printable part of a host name, bounded in length.
func deviceName(hostname string) string {
	name := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, strings.TrimSpace(hostname))
	if runes := []rune(name); len(runes) > deviceNameLimit {
		name = string(runes[:deviceNameLimit])
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "device"
	}
	return name
}

func newEnrollmentCode() (string, error) {
	limit := big.NewInt(int64(len(enrollmentCodeLetters)))
	code := make([]byte, enrollmentCodeLength)
	for i := range code {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		code[i] = enrollmentCodeLetters[n.Int64()]
	}
	return string(code), nil
}

// normalizeEnrollmentCode accepts the code as typed: any case, with or
// without the separator and surrounding spaces.
func normalizeEnrollmentCode(code string) string {
	code = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	if len(code) != enrollmentCodeLength || strings.Trim(code, enrollmentCodeLetters) != "" {
		return ""
	}
	return code
}

func enrollmentDigest(code string) string {
	if code == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
