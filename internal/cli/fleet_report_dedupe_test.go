package cli

import "testing"

func TestSplitMachineLine(t *testing.T) {
	m, rest := splitMachineLine("machine=abc|mac|jingyu|/Users/jingyu\n{\"ok\":true}\n")
	if m != "abc|mac|jingyu|/Users/jingyu" || rest != `{"ok":true}` {
		t.Fatalf("got %q %q", m, rest)
	}
	m, rest = splitMachineLine("machine=|||\n{}")
	if m != "" || rest != "{}" {
		t.Fatalf("empty key: %q %q", m, rest)
	}
	m, rest = splitMachineLine(`{"ok":true}`)
	if m != "" || rest != `{"ok":true}` {
		t.Fatalf("no machine line: %q %q", m, rest)
	}
}

func TestReportPrimariesCountsEachMachineOnce(t *testing.T) {
	// dostos-mac, local (hub), c1, hamburg (same as c1), paris (unknown key)
	keys := []string{"mac", "mac", "m1", "m1", ""}
	hubs := []bool{false, true, false, false, false}
	got := reportPrimaries(keys, hubs)
	want := []int{1, 1, 2, 2, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("primaries = %v, want %v", got, want)
		}
	}
}
