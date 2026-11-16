//go:build windows

package keys

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// setDACL replaces path's DACL from an SDDL string. protected controls the PROTECTED flag.
func setDACL(t *testing.T, path, sddl string, protected bool) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if protected {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func newKey(t *testing.T) (dir, key, user string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "cfg")
	if _, err := Generate(dir, "k", "k"); err != nil {
		t.Fatal(err)
	}
	u, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, "k"), u
}

func TestCreatedDirAndKeyPassCheck(t *testing.T) {
	dir, key, _ := newKey(t)
	if err := checkPathACL(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	if err := checkPathACL(dir); err != nil {
		t.Fatalf("dir: %v", err)
	}
	if _, err := LoadSigner(key); err != nil {
		t.Fatal(err)
	}
}

// An empty loose pre-existing dir has no tree to re-ACL, so it is repaired.
func TestEnsureDirRepairsEmptyLooseDir(t *testing.T) {
	d := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(d); err != nil {
		t.Fatal(err)
	}
	if err := checkPathACL(d); err != nil {
		t.Fatal(err)
	}
}

// A non-empty pre-existing dir is verified, never rewritten. A loose one (inherited ACEs) is
// refused and its DACL, and its children's, are left alone.
func TestEnsureDirRefusesLooseExistingDirWithoutRewriting(t *testing.T) {
	d := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(d, "child.txt")
	if err := os.WriteFile(child, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(d); err == nil {
		t.Fatal("loose pre-existing dir accepted")
	}
	if err := checkPathACL(d); err == nil {
		t.Fatal("loose dir was rewritten")
	}
	if err := checkPathACL(child); err == nil {
		t.Fatal("child was rewritten")
	}
}

// A pre-existing dir that already carries the private DACL passes, and so does a nested create.
func TestEnsureDirAcceptsPrivateExistingDir(t *testing.T) {
	d := filepath.Join(t.TempDir(), "priv")
	if err := EnsureDir(d); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(d); err != nil {
		t.Fatalf("second EnsureDir: %v", err)
	}
	if err := EnsureDir(filepath.Join(d, "a", "b")); err != nil {
		t.Fatal(err)
	}
	if err := checkPathACL(filepath.Join(d, "a", "b")); err != nil {
		t.Fatal(err)
	}
}

func TestSIDStringRejectsNil(t *testing.T) {
	if _, err := sidString(nil); err == nil {
		t.Fatal("nil SID accepted")
	}
	if s, err := currentUserSID(); err != nil || s == "" {
		t.Fatalf("current user SID %q %v", s, err)
	}
}

func TestEveryoneACERefused(t *testing.T) {
	_, key, user := newKey(t)
	setDACL(t, key, "D:P(A;;FA;;;"+user+")(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;WD)", true)
	if _, err := LoadSigner(key); err == nil {
		t.Fatal("key with Everyone ACE accepted")
	}
}

func TestUnprotectedDACLRefused(t *testing.T) {
	_, key, user := newKey(t)
	// Unprotected: Windows merges the parent's inheritable ACEs in as inherited ACEs.
	setDACL(t, key, "D:(A;;FA;;;"+user+")", false)
	if _, err := LoadSigner(key); err == nil {
		t.Fatal("unprotected/inherited DACL accepted")
	}
}

func TestDenyACERefused(t *testing.T) {
	_, key, user := newKey(t)
	setDACL(t, key, "D:P(D;;FW;;;WD)(A;;FA;;;"+user+")", true)
	if _, err := LoadSigner(key); err == nil {
		t.Fatal("deny ACE accepted")
	}
}

// A wrong owner cannot be simulated reliably: taking or assigning ownership needs
// SeTakeOwnership/SeRestore (admin) and a second account. The owner rule is covered by
// TestACLPolicy ("wrong owner") on every OS.

// The atomic writer's temp (and so the final file) carries the explicit private DACL, not
// the one inherited from its directory.
func TestWriteAtomic0600HasPrivateDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic0600(dir, "known_hosts", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := checkPathACL(filepath.Join(dir, "known_hosts")); err != nil {
		t.Fatal(err)
	}
}
