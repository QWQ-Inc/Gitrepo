package registry

import "testing"

func TestParseSlug(t *testing.T) {
	ok := map[string][2]string{
		"acme/demo":       {"acme", "demo"},
		"acme/demo.git":   {"acme", "demo"},
		"/Acme/my_app-2/": {"Acme", "my_app-2"},
		"a/b.c":           {"a", "b.c"},
	}
	for in, want := range ok {
		org, name, err := ParseSlug(in)
		if err != nil || org != want[0] || name != want[1] {
			t.Errorf("ParseSlug(%q) = %q %q %v", in, org, name, err)
		}
	}
	for _, bad := range []string{"demo", "a/b/c", "../x", "a/..", "a/.hidden", "api/demo", "a/x.lock", "a/b c", "-a/b", ""} {
		if _, _, err := ParseSlug(bad); err == nil {
			t.Errorf("ParseSlug(%q) accepted", bad)
		}
	}
}
