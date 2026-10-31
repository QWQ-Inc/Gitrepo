package sshserve

import (
	"reflect"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	for in, want := range map[string][]string{
		"git-upload-pack 'acme/demo.git'":           {"git-upload-pack", "acme/demo.git"},
		"git-receive-pack '/acme/demo.git'":         {"git-receive-pack", "/acme/demo.git"},
		"git upload-pack \"acme/my repo.git\"":      {"git", "upload-pack", "acme/my repo.git"},
		"git-lfs-authenticate acme/demo.git upload": {"git-lfs-authenticate", "acme/demo.git", "upload"},
		"  git-upload-archive   'acme/demo.git'   ": {"git-upload-archive", "acme/demo.git"},
		"git-upload-pack ''":                        {"git-upload-pack", ""},
	} {
		if got := splitCommand(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitCommand(%q) = %q, want %q", in, got, want)
		}
	}
}
