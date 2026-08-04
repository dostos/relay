package sshcoord

import (
	"strings"
	"testing"
)

func TestSensorCommandValidates(t *testing.T) {
	c := New()
	cmd, err := c.SensorCommand("sess1", "exit")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "relay service event emit") || !strings.Contains(cmd, "'sess1'") || !strings.Contains(cmd, "'exit'") {
		t.Fatalf("unexpected cmd %q", cmd)
	}
	if !strings.Contains(cmd, ">/dev/null 2>&1") {
		t.Fatalf("sensor emit must silence stdout/stderr, got %q", cmd)
	}
	if _, err := c.SensorCommand("../x", "exit"); err == nil {
		t.Fatal("expected session reject")
	}
	if _, err := c.SensorCommand("sess1", "x;rm"); err == nil {
		t.Fatal("expected kind reject")
	}
}
