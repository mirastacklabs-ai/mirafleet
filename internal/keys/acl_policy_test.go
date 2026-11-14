package keys

import "testing"

func TestACLPolicy(t *testing.T) {
	const me = "S-1-5-21-1-2-3-1001"
	const everyone = "S-1-1-0"
	ok := func() aclInfo {
		return aclInfo{Owner: me, Protected: true, HasDACL: true, ACEs: []aceEntry{
			{SID: me}, {SID: sidSystem}, {SID: sidAdmins},
		}}
	}
	if err := aclPolicy(ok(), me); err != nil {
		t.Fatalf("good ACL refused: %v", err)
	}
	cases := map[string]func(*aclInfo){
		"wrong owner":     func(a *aclInfo) { a.Owner = sidAdmins },
		"null dacl":       func(a *aclInfo) { a.HasDACL = false },
		"unprotected":     func(a *aclInfo) { a.Protected = false },
		"everyone":        func(a *aclInfo) { a.ACEs = append(a.ACEs, aceEntry{SID: everyone}) },
		"other user":      func(a *aclInfo) { a.ACEs = append(a.ACEs, aceEntry{SID: "S-1-5-21-1-2-3-1002"}) },
		"inherited":       func(a *aclInfo) { a.ACEs[0].Inherited = true },
		"deny ace":        func(a *aclInfo) { a.ACEs = append(a.ACEs, aceEntry{Type: 1, SID: everyone}) },
		"unknown ace":     func(a *aclInfo) { a.ACEs = append(a.ACEs, aceEntry{Type: 9}) },
		"empty allow sid": func(a *aclInfo) { a.ACEs = append(a.ACEs, aceEntry{}) },
	}
	for name, mut := range cases {
		a := ok()
		mut(&a)
		if err := aclPolicy(a, me); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
