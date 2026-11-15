package opconf

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectRemoteWorkdir(t *testing.T) {
	good := map[string]Project{
		"remote: true\n": {Remote: true},
		"fleet: f\ncontext: c\nremote: true\nworkdir: w1\n": {Fleet: "f", Context: "c", Remote: true, Workdir: "w1"},
		"remote: false\n": {},
		"fleet: f\n":      {Fleet: "f"},
	}
	for body, want := range good {
		d := t.TempDir()
		write(t, filepath.Join(d, ProjectName), body)
		p, _, err := LoadProject(d, d)
		if err != nil || p != want {
			t.Errorf("%q: %+v %v", body, p, err)
		}
	}
	bad := []string{
		"workdir: w1\n",                 // needs remote
		"remote: true\nworkdir: ../x\n", // not a name
		"remote: true\nworkdir: A B\n",  // not a name
		"remote: yes please\n",          // not a bool
		"remote: true\nremote: true\n",  // duplicate key
		"remote: true\naddr: 1.2.3.4\n", // no addresses
		"remote: true\nkey: x\n",        // no keys
		"remote: true\n---\nfleet: f\n", // second document
		"remote: [true]\n",
		"workdir: {a: b}\nremote: true\n",
	}
	for _, body := range bad {
		d := t.TempDir()
		write(t, filepath.Join(d, ProjectName), body)
		if _, _, err := LoadProject(d, d); err == nil {
			t.Errorf("%q accepted", body)
		} else if strings.Contains(err.Error(), "1.2.3.4") {
			t.Errorf("error echoes a value: %v", err)
		}
	}
}
