package actions

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDescribe(t *testing.T) {
	s := mustParse(t)
	d := s.Describe()
	var names []string
	for _, a := range d {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "fail,hello,plain,slow,tail" {
		t.Fatalf("names (sorted): %v", names)
	}
	if len(d[1].Params) != 1 || d[1].Params[0].Name != "name" || d[1].Params[0].Pattern != "[a-z]{1,8}" || d[1].Params[0].Default != nil {
		t.Fatalf("hello: %+v", d[1])
	}
	if p := d[4].Params; len(p) != 1 || p[0].Default == nil || *p[0].Default != "200" {
		t.Fatalf("tail: %+v", d[4])
	}
	if len(d[2].Params) != 0 {
		t.Fatalf("plain: %+v", d[2])
	}
	// Nothing sensitive: no argv, paths, env or timeouts in the output.
	b, _ := json.Marshal(d)
	for _, bad := range []string{"/bin", "/usr", "echo", "tail-it", "sleep", "PATH", "--tag"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("Describe leaks %q: %s", bad, b)
		}
	}
	// The result is a copy: mutating a default must not change the set.
	*d[4].Params[0].Default = "x"
	if *s.Actions["tail"].Params["lines"].Default != "200" {
		t.Fatal("Describe aliases the set's defaults")
	}
}

func TestDescribeParamOrderDeterministic(t *testing.T) {
	s, err := Parse([]byte(`{actions: {a: {run: ["/bin/echo","{{z}}","{{b}}","{{m}}"], params: {z: {pattern: "z"}, b: {pattern: "b"}, m: {pattern: "m"}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		p := s.Describe()[0].Params
		if len(p) != 3 || p[0].Name != "b" || p[1].Name != "m" || p[2].Name != "z" {
			t.Fatalf("order: %+v", p)
		}
	}
}
