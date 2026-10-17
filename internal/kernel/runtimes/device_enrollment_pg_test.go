package runtimes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func deviceEnrollmentStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	pool := testutil.PostgresPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws')`); err != nil {
		t.Fatal(err)
	}
	return NewStore(pool), ctx
}

func TestDeviceEnrollmentBindsANewDeviceToThePersonOnce(t *testing.T) {
	store, ctx := deviceEnrollmentStore(t)
	enrollment, err := store.IssueDeviceEnrollment(ctx, "ws", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(enrollment.Code) != 9 || enrollment.Code[4] != '-' {
		t.Fatalf("code = %q, want XXXX-XXXX", enrollment.Code)
	}
	// Typed in lower case without the separator, the code still works.
	typed := strings.ToLower(strings.ReplaceAll(enrollment.Code, "-", ""))
	device, err := store.EnrollDevice(ctx, " "+typed+" ", "Alice's MacBook", "")
	if err != nil {
		t.Fatal(err)
	}
	if device.Name != "Alice's MacBook" || device.RuntimeID == "" || device.Token == "" {
		t.Fatalf("device = %+v", device)
	}
	runtime, err := store.ValidateToken(ctx, device.Token)
	if err != nil || runtime.ID != device.RuntimeID {
		t.Fatalf("token validates as %+v, %v", runtime, err)
	}
	var owner string
	if err := store.pool.QueryRow(ctx, `SELECT device_owner_user_id FROM weave_runtimes WHERE id=$1`, device.RuntimeID).Scan(&owner); err != nil || owner != "alice" {
		t.Fatalf("device owner = %q, %v", owner, err)
	}
	if _, err := store.EnrollDevice(ctx, enrollment.Code, "Another Mac", ""); !errors.Is(err, ErrEnrollmentCodeUsed) {
		t.Fatalf("second use: %v, want ErrEnrollmentCodeUsed", err)
	}
}

func TestDeviceEnrollmentRefusesUnknownAndExpiredCodes(t *testing.T) {
	store, ctx := deviceEnrollmentStore(t)
	for _, code := range []string{"", "ABCD", "ABCD-EFG1", "ABCD-EFGH"} {
		if _, err := store.EnrollDevice(ctx, code, "mac", ""); !errors.Is(err, ErrEnrollmentCodeInvalid) {
			t.Fatalf("code %q: %v, want ErrEnrollmentCodeInvalid", code, err)
		}
	}
	issued := time.Now()
	store.clock = func() time.Time { return issued }
	enrollment, err := store.IssueDeviceEnrollment(ctx, "ws", "alice")
	if err != nil {
		t.Fatal(err)
	}
	store.clock = func() time.Time { return issued.Add(DeviceEnrollmentTTL) }
	if _, err := store.EnrollDevice(ctx, enrollment.Code, "mac", ""); !errors.Is(err, ErrEnrollmentCodeExpired) {
		t.Fatalf("expired code: %v, want ErrEnrollmentCodeExpired", err)
	}
}

func TestDeviceReEnrollmentKeepsTheDeviceAndReplacesItsToken(t *testing.T) {
	store, ctx := deviceEnrollmentStore(t)
	first, err := store.IssueDeviceEnrollment(ctx, "ws", "alice")
	if err != nil {
		t.Fatal(err)
	}
	device, err := store.EnrollDevice(ctx, first.Code, "mac", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.IssueDeviceEnrollment(ctx, "ws", "alice")
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.EnrollDevice(ctx, second.Code, "renamed host", device.RuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	if again.RuntimeID != device.RuntimeID || again.Name != "mac" {
		t.Fatalf("re-enrolled as %+v, want the same device %q named mac", again, device.RuntimeID)
	}
	if _, err := store.ValidateToken(ctx, device.Token); err == nil {
		t.Fatal("the replaced token still validates")
	}
	if _, err := store.ValidateToken(ctx, again.Token); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceEnrollmentCannotTakeAnotherPersonsDevice(t *testing.T) {
	store, ctx := deviceEnrollmentStore(t)
	aliceCode, err := store.IssueDeviceEnrollment(ctx, "ws", "alice")
	if err != nil {
		t.Fatal(err)
	}
	alices, err := store.EnrollDevice(ctx, aliceCode.Code, "mac", "")
	if err != nil {
		t.Fatal(err)
	}
	bobCode, err := store.IssueDeviceEnrollment(ctx, "ws", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnrollDevice(ctx, bobCode.Code, "mac", alices.RuntimeID); !errors.Is(err, ErrRuntimeBindingMismatch) {
		t.Fatalf("taking alice's device: %v, want ErrRuntimeBindingMismatch", err)
	}
	if _, err := store.ValidateToken(ctx, alices.Token); err != nil {
		t.Fatalf("alice's token stopped working: %v", err)
	}
	// The refused exchange left Bob's code unspent.
	bobs, err := store.EnrollDevice(ctx, bobCode.Code, "mac", "")
	if err != nil {
		t.Fatal(err)
	}
	if bobs.RuntimeID == alices.RuntimeID {
		t.Fatal("bob was given alice's device")
	}
}

func TestDeviceEnrollmentNamesAroundExistingRuntimes(t *testing.T) {
	store, ctx := deviceEnrollmentStore(t)
	if _, _, err := store.Create(ctx, "ws", "Studio"); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for range 2 {
		enrollment, err := store.IssueDeviceEnrollment(ctx, "ws", "alice")
		if err != nil {
			t.Fatal(err)
		}
		device, err := store.EnrollDevice(ctx, enrollment.Code, "  Studio\n", "")
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, device.Name)
	}
	if names[0] != "Studio (2)" || names[1] != "Studio (3)" {
		t.Fatalf("names = %q", names)
	}
	enrollment, err := store.IssueDeviceEnrollment(ctx, "ws", "alice")
	if err != nil {
		t.Fatal(err)
	}
	unnamed, err := store.EnrollDevice(ctx, enrollment.Code, "\t", "")
	if err != nil || unnamed.Name != "device" {
		t.Fatalf("blank host name gave %q, %v", unnamed.Name, err)
	}
}
