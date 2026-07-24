package core

import "testing"

func TestDecideNextMatrix(t *testing.T) {
	cases := []struct {
		kind     HandoffKind
		ev       string
		timedOut bool
		want     string
	}{
		{KindJob, "exit", false, "done"},
		{KindAgent, "exit", false, "done"},
		{KindJob, "idle", false, "wait"},
		{KindAgent, "idle", false, "send"},
		{KindJob, "needs_input", false, "escalate"},
		{KindAgent, "needs_input", false, "send"},
		{KindJob, "started", false, "wait"},
		{KindAgent, "", true, "wait"},
		{KindJob, "", true, "wait"},
	}
	for _, tc := range cases {
		got, _ := DecideNext(tc.kind, tc.ev, tc.timedOut)
		if got != tc.want {
			t.Fatalf("kind=%s ev=%q timeout=%v got=%s want=%s", tc.kind, tc.ev, tc.timedOut, got, tc.want)
		}
	}
}

func TestArgvForWaitAndDone(t *testing.T) {
	a := argvFor("wait", "ho-1", "sess-1")
	if len(a) < 5 || a[0] != "relay" || a[2] != "wait" {
		t.Fatalf("bad wait argv %#v", a)
	}
	d := argvFor("done", "ho-1", "sess-1")
	if d[2] != "done" {
		t.Fatalf("bad done argv %#v", d)
	}
	if argvFor("null", "ho-1", "sess-1") != nil {
		t.Fatal("null should have no argv")
	}
}
