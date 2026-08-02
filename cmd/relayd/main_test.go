package main

import "testing"

func TestVizBrokerRefusesCommandsOutsideProjectionProtocol(t *testing.T) {
	t.Setenv("SSH_ORIGINAL_COMMAND", "relayd status")
	if code := cmdVizBroker([]string{"--service", "relay-viz-mac"}); code != 126 {
		t.Fatalf("broker exit=%d, want refusal", code)
	}
}

func TestVizBrokerRequiresPinnedService(t *testing.T) {
	t.Setenv("SSH_ORIGINAL_COMMAND", "viz-subscribe relay-viz-mac 0 0")
	if code := cmdVizBroker([]string{"--service", "other"}); code != 2 {
		t.Fatalf("broker exit=%d, want invalid configuration", code)
	}
}
