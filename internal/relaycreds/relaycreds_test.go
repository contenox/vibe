package relaycreds_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/contenox/contenox/internal/relaycreds"
)

func sample() relaycreds.Credentials {
	return relaycreds.Credentials{
		Endpoint:       "https://relay.invalid",
		InstanceToken:  "secret-token",
		InstanceID:     "inst-a",
		AccountID:      "acct-1",
		RelayPublicKey: "Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyYWI=",
	}
}

// TestUnit_SaveAndLoadRoundTrip is the whole of the storage contract.
func TestUnit_SaveAndLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), ".contenox")
	if err := relaycreds.Save(dir, sample()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := relaycreds.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != sample() {
		t.Fatalf("Load = %+v, want %+v", got, sample())
	}
}

// TestUnit_TokenFileIsNotReadableByOthers: the instance token is a secret, and
// a copied file is a copied identity.
func TestUnit_TokenFileIsNotReadableByOthers(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are advisory on windows")
	}
	dir := filepath.Join(t.TempDir(), ".contenox")
	if err := relaycreds.Save(dir, sample()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(relaycreds.Path(dir))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("credential file is %v, want no group or other access", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("credential directory is %v, want no group or other access", perm)
	}
}

// TestUnit_LoadWithoutEnrolmentIsNotAFailure keeps "nobody has logged in" the
// ordinary state it is: a runtime that sees it carries on without a relay.
func TestUnit_LoadWithoutEnrolmentIsNotAFailure(t *testing.T) {
	t.Parallel()
	if _, err := relaycreds.Load(t.TempDir()); !errors.Is(err, relaycreds.ErrNotEnrolled) {
		t.Fatalf("Load = %v, want ErrNotEnrolled", err)
	}
}

// TestUnit_SaveReplacesAndDeleteIsIdempotent: a machine has one identity at a
// relay, and logging out twice is not an error.
func TestUnit_SaveReplacesAndDeleteIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), ".contenox")
	if err := relaycreds.Save(dir, sample()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	second := sample()
	second.InstanceID = "inst-b"
	if err := relaycreds.Save(dir, second); err != nil {
		t.Fatalf("Save again: %v", err)
	}
	got, err := relaycreds.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.InstanceID != "inst-b" {
		t.Fatalf("InstanceID = %q, want the second enrolment", got.InstanceID)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d files, want only the credential", len(entries))
	}
	for range 2 {
		if err := relaycreds.Delete(dir); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if _, err := relaycreds.Load(dir); !errors.Is(err, relaycreds.ErrNotEnrolled) {
		t.Fatalf("Load after Delete = %v, want ErrNotEnrolled", err)
	}
}
