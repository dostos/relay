package cli

import (
	"fmt"
	"strings"

	"github.com/dostos/relay/internal/agentguide"
)

// cmdAgents is `relay agents install|status|doctor` (#76). It always acts on
// THIS machine (never forwarded through the desktop bridge).
func (a *App) cmdAgents(args []string) int {
	if len(args) == 0 {
		return a.helpFor([]string{"agents"}, 2)
	}
	verb := args[0]
	var o agentguide.Options
	for _, f := range args[1:] {
		switch {
		case f == "--dry-run" && verb == "install":
			o.DryRun = true
		case f == "--all" && verb == "install":
			o.All = true
		case f == "--force" && verb == "install":
			o.Force = true
		case f == "--no-path" && verb == "install":
			o.NoPath = true
		default:
			return a.failStructured(rejectUnknownFlag(f), map[string]any{"reason": "usage", "argv": []string{"relay", "agents", verb, "--help"}})
		}
	}
	e := agentguide.DefaultEnv()
	switch verb {
	case "status":
		r := agentguide.Status(e)
		if a.JSON {
			_ = a.out(r)
		} else {
			printAgentsResult(r)
		}
		return boolExit(r.OK)
	case "install":
		r := agentguide.Install(e, o)
		if a.JSON {
			_ = a.out(r)
		} else {
			printAgentsResult(r)
		}
		return boolExit(r.OK)
	case "doctor":
		d := agentguide.Doctor(e)
		if a.JSON {
			_ = a.out(d)
		} else {
			fmt.Printf("relay binary: %s\n", orDash(d.Relay))
			for _, c := range d.Path {
				fmt.Printf("  path %-20s %-5v %s%s\n", c.Check, c.OK, c.Shell, suffix(c.Found, c.Error))
			}
			for _, ag := range d.Agents {
				if !ag.Present {
					fmt.Printf("  agent %-13s not installed\n", ag.Agent)
					continue
				}
				fmt.Printf("  agent %-13s relay_on_path=%v sees_guidance=%v %s\n", ag.Agent, ag.RelayOnPath, ag.SeesGuidance, strings.Join(ag.GuidancePaths, ","))
			}
			for _, p := range d.Problems {
				fmt.Println("  problem: " + p)
			}
			if len(d.Next) > 0 {
				fmt.Println("  next: " + joinArgv(d.Next))
			}
		}
		return boolExit(d.OK)
	default:
		return a.failStructured(fmt.Errorf("unknown agents verb %q (install|status|doctor)", verb), map[string]any{"reason": "usage", "argv": []string{"relay", "agents", "--help"}})
	}
}

func printAgentsResult(r agentguide.Result) {
	fmt.Printf("guidance v%s (%s) dry_run=%v changed=%v\n", r.Version, r.Hash, r.DryRun, r.Changed)
	for _, ag := range r.Agents {
		fmt.Printf("  agent %-13s present=%v %s\n", ag.Name, ag.Present, ag.Binary)
	}
	for _, t := range r.Targets {
		fmt.Printf("  %-22s %-9s %-18s %s\n", t.ID, t.State, orDash(t.Action), t.Path)
	}
	for _, c := range r.Path {
		fmt.Printf("  path %-20s ok=%v %s\n", c.Check, c.OK, c.Shell)
	}
	for _, f := range r.PathFix {
		fmt.Printf("  path-fix %s → %s %s\n", f.Check, f.File, f.Action)
	}
	for _, p := range r.Problems {
		fmt.Println("  problem: " + p)
	}
	if len(r.Next) > 0 {
		fmt.Println("  next: " + joinArgv(r.Next))
	}
}

func boolExit(ok bool) int {
	if ok {
		return 0
	}
	return 1
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func suffix(found, errText string) string {
	if found != "" {
		return " → " + found
	}
	if errText != "" {
		return " (" + errText + ")"
	}
	return ""
}
