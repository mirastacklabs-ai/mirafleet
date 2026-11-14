//go:build windows

package keys

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows protection: every directory and private file is created with a protected DACL passed in
// SECURITY_ATTRIBUTES, so the inherited DACL never applies, even briefly. The owner is the current
// user and the only allow-ACEs are the current user, SYSTEM and BUILTIN\Administrators.
// checkPathACL re-verifies all of that on load.

// ACE_HEADER.AceFlags bit marking an inherited ACE.
const aceInheritedFlag = 0x10

func init() {
	mkdirPrivate = winMkdirPrivate
	createExcl = winCreateExcl
}

func currentUserSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("current user: %w", err)
	}
	return sidString(u.User.Sid)
}

// sidString renders a SID. (*SID).String() returns "" on failure; an empty SID is an error, never
// a value (an empty owner/ACE SID must not compare equal to anything, F-14).
func sidString(s *windows.SID) (string, error) {
	if s == nil {
		return "", fmt.Errorf("nil SID")
	}
	str := s.String()
	if str == "" {
		return "", fmt.Errorf("cannot convert SID to string")
	}
	return str, nil
}

// privateSDDL is the protected owner-only descriptor. container adds object/container inherit
// flags so a directory's future children get the same ACEs.
func privateSDDL(user string, container bool) string {
	flags := ""
	if container {
		flags = "OICI"
	}
	return fmt.Sprintf("O:%sD:P(A;%s;FA;;;%s)(A;%s;FA;;;SY)(A;%s;FA;;;BA)", user, flags, user, flags, flags)
}

func privateSA(container bool) (*windows.SecurityAttributes, error) {
	user, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(privateSDDL(user, container))
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, nil
}

// winMkdirPrivate is MkdirAll with the protected DACL applied at creation to every missing
// component. Unlike Unix it cannot tighten a pre-existing dir; it verifies it instead.
func winMkdirPrivate(dir string) error {
	dir = filepath.Clean(dir)
	var missing []string
	for d := dir; ; {
		if st, err := os.Stat(d); err == nil {
			if !st.IsDir() {
				return &os.PathError{Op: "mkdir", Path: d, Err: windows.ERROR_DIRECTORY}
			}
			break
		}
		missing = append(missing, d)
		p := filepath.Dir(d)
		if p == d {
			break
		}
		d = p
	}
	sa, err := privateSA(true)
	if err != nil {
		return err
	}
	// Every directory this call touches is verified (owner, protected DACL, only our ACEs) and
	// only components created by this call are rewritten (they already carry the protected DACL
	// from CreateDirectory). One that lost a create race (ALREADY_EXISTS, F-12) or that pre-existed
	// is only checked, owner included. A pre-existing dir that fails the check is repaired only
	// when it is empty (F-13: SetNamedSecurityInfo with OICI re-ACLs the whole tree under it);
	// otherwise it is refused. The owner is never changed, so a foreign-owned dir stays refused.
	verify := missing
	if len(missing) == 0 {
		verify = []string{dir}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		p, err := windows.UTF16PtrFromString(missing[i])
		if err != nil {
			return err
		}
		if err := windows.CreateDirectory(p, sa); err != nil && err != windows.ERROR_ALREADY_EXISTS {
			return &os.PathError{Op: "mkdir", Path: missing[i], Err: err}
		}
	}
	for _, d := range verify {
		err := checkPathACL(d)
		if err != nil && len(missing) == 0 {
			if ents, rerr := os.ReadDir(d); rerr == nil && len(ents) == 0 {
				dacl, _, derr := sa.SecurityDescriptor.DACL()
				if derr != nil {
					return derr
				}
				if serr := windows.SetNamedSecurityInfo(d, windows.SE_FILE_OBJECT,
					windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
					nil, nil, dacl, nil); serr != nil {
					return &os.PathError{Op: "protect", Path: d, Err: serr}
				}
				err = checkPathACL(d)
				// A file dropped in during the repair would have been re-ACLed by it: refuse.
				if err == nil {
					if ents, rerr := os.ReadDir(d); rerr != nil || len(ents) != 0 {
						err = fmt.Errorf("directory changed during repair")
					}
				}
			}
		}
		if err != nil {
			return fmt.Errorf("directory %s is not private: %w (fix its permissions or remove it)", d, err)
		}
	}
	return nil
}

// winCreateExcl creates path exclusively (CREATE_NEW). Files without group/other bits in mode
// get the protected DACL at creation; others (the .pub file) keep the inherited default.
func winCreateExcl(path string, mode os.FileMode) (*os.File, error) {
	var sa *windows.SecurityAttributes
	if mode.Perm()&0o077 == 0 {
		var err error
		if sa, err = privateSA(false); err != nil {
			return nil, err
		}
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// checkPathACL reads path's owner and DACL and applies aclPolicy.
func checkPathACL(path string) error {
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read security descriptor of %s: %w", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("private key %s: cannot read owner: %v", path, err)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("private key %s: cannot read descriptor control: %w", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("private key %s: cannot read DACL: %w", path, err)
	}
	ownerStr, err := sidString(owner)
	if err != nil {
		return fmt.Errorf("%s: owner: %w", path, err)
	}
	info := aclInfo{
		Owner:     ownerStr,
		Protected: ctl&windows.SE_DACL_PROTECTED != 0,
		HasDACL:   dacl != nil,
	}
	if dacl != nil {
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				return fmt.Errorf("private key %s: cannot read ACE %d: %w", path, i, err)
			}
			e := aceEntry{Type: ace.Header.AceType, Inherited: ace.Header.AceFlags&aceInheritedFlag != 0}
			// Only ACCESS_ALLOWED_ACE_TYPE has the SID layout read here; for other types the SID
			// stays empty and aclPolicy rejects the entry by type.
			if e.Type == aceTypeAllow {
				if e.SID, err = sidString((*windows.SID)(unsafe.Pointer(&ace.SidStart))); err != nil {
					return fmt.Errorf("%s: ACE %d: %w", path, i, err)
				}
			}
			info.ACEs = append(info.ACEs, e)
		}
	}
	if err := aclPolicy(info, user); err != nil {
		return fmt.Errorf("private key %s is not private to the current user: %w", path, err)
	}
	return nil
}
