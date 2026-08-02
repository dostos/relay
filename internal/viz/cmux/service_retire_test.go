package cmux

import "testing"

func TestLegacyBridgeCommandRequiresExactExecutableAndSubcommand(t *testing.T) {
	for _, command := range []string{
		"/Users/test/.local/bin/relayd bridge --relay-bin /Users/test/.local/bin/relay",
		"relayd bridge",
	} {
		if !isLegacyBridgeCommand(command) {
			t.Fatalf("expected legacy bridge command: %q", command)
		}
	}
	for _, command := range []string{
		"/Users/test/.local/bin/relayd viz follow",
		"/Users/test/.local/bin/relay supervise",
		"sh -c relayd bridge",
		"relayd-evil bridge",
	} {
		if isLegacyBridgeCommand(command) {
			t.Fatalf("accepted unrelated command: %q", command)
		}
	}
}
