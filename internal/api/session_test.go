package api

import (
	"strings"
	"testing"
)

func TestValidateSession(t *testing.T) {
	if MaxSessionLen != 128 {
		t.Fatalf("MaxSessionLen = %d", MaxSessionLen)
	}
	for _, ok := range []string{"a", "s1", "A.b_c:d-e", strings.Repeat("x", 128)} {
		if e := ValidateSession(ok); e != nil {
			t.Errorf("%q refused: %v", ok, e)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 129), "a b", "a/b", "a\n", "é", "a\x00"} {
		if e := ValidateSession(bad); e == nil || e.Code != CodeBadRequest {
			t.Errorf("%q accepted", bad)
		}
	}
}
