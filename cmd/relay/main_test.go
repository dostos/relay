package main

import (
	"reflect"
	"testing"

	"github.com/dostos/relay/internal/compat"
)

func TestLegacyRelaydCompatibilityRoutesToPrimaryBinary(t *testing.T) {
	for _, tc := range []struct {
		legacy []string
		want   []string
	}{
		{[]string{"serve"}, []string{"service", "event", "run"}},
		{[]string{"control", "serve"}, []string{"service", "boundary", "run"}},
		{[]string{"emit", "-s", "worker", "--kind", "idle"}, []string{"service", "event", "emit", "-s", "worker", "--kind", "idle"}},
		{[]string{"subscribe", "-s", "worker", "-f"}, []string{"service", "event", "subscribe", "-s", "worker", "-f"}},
		{[]string{"viz", "follow"}, []string{"viz", "serve"}},
		{[]string{"viz-broker", "--service", "relay-viz-mac"}, []string{"viz-broker", "--service", "relay-viz-mac"}},
	} {
		got, ok := compat.MapRelayd(tc.legacy)
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("legacy=%v mapped=%v ok=%t want=%v", tc.legacy, got, ok, tc.want)
		}
	}
}

func TestLegacyRelaydCompatibilityIsNarrow(t *testing.T) {
	if got, ok := compat.MapRelayd([]string{"shell", "rm", "-rf"}); ok || got != nil {
		t.Fatalf("unexpected compatibility route: %v", got)
	}
}
