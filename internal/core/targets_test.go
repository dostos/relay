package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSSHConfigIncludeAndFilter(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "config")
	inc := filepath.Join(dir, "extra.conf")
	if err := os.WriteFile(inc, []byte(`
Host c1
  HostName 10.0.0.1
  User alice
  IdentityFile ~/.ssh/id_ed25519
  ProxyJump bastion

Host github.com
  HostName github.com
  User git

Host "*.lab"
  HostName %h
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte(`
Include `+inc+`

Host *
  ForwardAgent yes

Host c3
  HostName 10.0.0.3
  User bob
	Port 2222
`), 0o644); err != nil {
		t.Fatal(err)
	}

	blocks, err := parseSSHConfigFiles([]string{main}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	byID := map[string]sshHostBlock{}
	for _, b := range blocks {
		for _, a := range b.aliases {
			if !usableHostAlias(a) {
				continue
			}
			ids = append(ids, a)
			byID[a] = b
		}
	}
	// Dedupe happens in ListTargets; here we only check parse + filter helpers.
	if !containsStr(ids, "c1") || !containsStr(ids, "c3") {
		t.Fatalf("want c1,c3 in %v", ids)
	}
	c1 := byID["c1"]
	if c1.hostname != "10.0.0.1" || c1.user != "alice" || !c1.proxyJump || !c1.identityFile {
		t.Fatalf("c1 block wrong: %+v", c1)
	}
	if byID["c3"].port != 2222 {
		t.Fatalf("c3 port = %d", byID["c3"].port)
	}
	if _, ok := byID["github.com"]; ok {
		t.Fatal("github.com should be filtered")
	}
	if _, ok := byID["*.lab"]; ok {
		t.Fatal("pattern host should be filtered")
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestUsableHostAlias(t *testing.T) {
	cases := map[string]bool{
		"c1":         true,
		"hamburg":    true,
		"*":          false,
		"*.lab":      false,
		"foo?":       false,
		"github.com": false,
		"":           false,
	}
	for in, want := range cases {
		if got := usableHostAlias(in); got != want {
			t.Fatalf("usableHostAlias(%q)=%v want %v", in, got, want)
		}
	}
}
