package auth

import "testing"

func TestRoleAtLeast(t *testing.T) {
	order := []Role{RoleUser, RoleAdmin, RoleOwner, RoleRoot}
	for i, have := range order {
		for j, min := range order {
			if got := have.AtLeast(min); got != (i >= j) {
				t.Fatalf("%s.AtLeast(%s) = %v", have, min, got)
			}
		}
	}
	if Role("").AtLeast(RoleUser) || Role("bogus").AtLeast(RoleUser) {
		t.Fatal("an unknown role must hold no privilege")
	}
	if RoleRoot.AtLeast(Role("")) {
		t.Fatal("an unknown role must not be a privilege threshold")
	}
	if !RoleRoot.Valid() {
		t.Fatal("root is a persisted role")
	}
}
