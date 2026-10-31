package auth

import (
	"strings"
	"testing"
)

func TestPrincipalCan(t *testing.T) {
	ci := &Principal{Name: "ci", Scopes: []string{ScopeRead}, Repos: []string{"acme/*"}}
	dev := &Principal{Name: "dev", Scopes: []string{ScopeCreate}}
	cases := []struct {
		p     *Principal
		scope string
		slug  string
		want  bool
	}{
		{ci, ScopeRead, "acme/demo", true},
		{ci, ScopeRead, "other/demo", false},
		{ci, ScopeWrite, "acme/demo", false},
		{dev, ScopeWrite, "any/thing", true},
		{dev, ScopeCreate, "any/thing", true},
		{dev, ScopeAdmin, "any/thing", false},
		{Admin(), ScopeAdmin, "x/y", true},
		{nil, ScopeRead, "x/y", false},
	}
	for _, c := range cases {
		name := "nil"
		if c.p != nil {
			name = c.p.Name
		}
		if got := c.p.Can(c.scope, c.slug); got != c.want {
			t.Errorf("%s.Can(%s, %s) = %v, want %v", name, c.scope, c.slug, got, c.want)
		}
	}
}

func TestTokens(t *testing.T) {
	plain, hash := NewToken()
	if !strings.HasPrefix(plain, TokenPrefix) || Hash(plain) != hash || len(hash) != 64 {
		t.Fatalf("bad token %q / %q", plain, hash)
	}
	if _, err := ParseScopes("read,bogus"); err == nil {
		t.Fatal("accepted unknown scope")
	}
	if s, err := ParseScopes(" write , read,write"); err != nil || len(s) != 2 {
		t.Fatalf("ParseScopes: %v %v", s, err)
	}
}
