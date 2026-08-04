// Package compat contains narrow argument-only compatibility routing.
package compat

// MapRelayd maps a supported legacy relayd invocation to the primary relay
// command surface. It deliberately contains no semantic authorization logic.
func MapRelayd(args []string) ([]string, bool) {
	if len(args) == 0 {
		return nil, false
	}
	switch args[0] {
	case "serve":
		return []string{"service", "event", "run"}, true
	case "bridge":
		return []string{"service", "run"}, true
	case "ping", "status", "emit", "subscribe":
		return append([]string{"service", "event"}, args...), true
	case "control":
		if len(args) == 2 && args[1] == "serve" {
			return []string{"service", "boundary", "run"}, true
		}
	case "viz":
		if len(args) == 2 && (args[1] == "follow" || args[1] == "sync") {
			return []string{"viz", "serve"}, true
		}
		if len(args) > 1 && args[1] == "authorize" {
			return args, true
		}
	case "viz-broker":
		return args, true
	case "version", "-V", "--version":
		return []string{"version"}, true
	case "build":
		return []string{"build"}, true
	}
	return nil, false
}
