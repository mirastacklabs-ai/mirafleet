package osb

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// InitPKIAt is InitPKI for a caller that already holds a verified directory (an fd opened
// O_DIRECTORY|O_NOFOLLOW and checked through that fd): every file is created with
// openat(dirfd, name, O_CREAT|O_EXCL|O_NOFOLLOW), so nothing is resolved by path again (F-19).
// path is only used to build the returned file names. The directory must be mode 0700 and owned
// by the effective uid, re-checked here through the fd.
func InitPKIAt(dir *os.File, path string, days int, now time.Time) (*PKI, error) {
	if days < 1 || days > 3650 {
		return nil, fmt.Errorf("days %d out of range (1..3650)", days)
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("%s is not a directory", path)
	}
	if st.Mode&0o077 != 0 {
		return nil, fmt.Errorf("%s: mode %04o is too open (want 0700)", path, st.Mode&0o7777)
	}
	if int(st.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("%s: owned by uid %d, want the effective uid %d", path, st.Uid, os.Geteuid())
	}
	return mintPKI(path, days, now, fdSink{dir})
}

type fdSink struct{ dir *os.File }

func (s fdSink) create(name string, b []byte) error {
	fd, err := unix.Openat(int(s.dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return &os.PathError{Op: "openat", Path: name, Err: err}
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err := f.Write(b); err != nil {
		f.Close()
		unix.Unlinkat(int(s.dir.Fd()), name, 0)
		return err
	}
	if err := f.Close(); err != nil {
		unix.Unlinkat(int(s.dir.Fd()), name, 0)
		return err
	}
	return nil
}

func (s fdSink) remove(name string) { unix.Unlinkat(int(s.dir.Fd()), name, 0) }
