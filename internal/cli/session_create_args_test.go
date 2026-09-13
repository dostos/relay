package cli

import (
	"strings"
	"testing"
)

func TestSessionCreateArgsTrailingCommandIsQuotedPerWord(t *testing.T) {
	opts, err := parseSessionCreateArgs("hamburg", []string{
		"--container", "hammer", "--ephemeral", "--name", "order-42",
		"--volume", "ev:/out", "--bind", "/data/in:/in:ro", "--gpus", "0,1", "--network", "host",
		"--", "/opt/worker/entrypoint", "--order-file", "/out/order.json", "--goal", "fix the flaky test; keep it green",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.HostID != "hamburg" || opts.Container != "hammer" || !opts.ContainerEphemeral || opts.Name != "order-42" {
		t.Errorf("basics: %+v", opts)
	}
	if len(opts.ContainerVolumes) != 1 || opts.ContainerVolumes[0] != "ev:/out" ||
		len(opts.ContainerBinds) != 1 || opts.ContainerBinds[0] != "/data/in:/in:ro" ||
		opts.ContainerGPUs != "0,1" || opts.ContainerNetwork != "host" {
		t.Errorf("extras: %+v", opts)
	}
	// Every word is quoted on its own, so the goal — which carries a space
	// and a semicolon — arrives as ONE argv word and the remote shell can
	// neither split it nor run its tail as a command.
	want := "'/opt/worker/entrypoint' '--order-file' '/out/order.json' '--goal' 'fix the flaky test; keep it green'"
	if opts.Command != want {
		t.Errorf("command:\n got %q\nwant %q", opts.Command, want)
	}
	if !strings.Contains(opts.Command, "'fix the flaky test; keep it green'") {
		t.Errorf("goal must be one quoted word: %q", opts.Command)
	}
}

func TestSessionCreateArgsWithoutCommandLeavesTheDefaultShell(t *testing.T) {
	opts, err := parseSessionCreateArgs("h", []string{"--repo", "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Command != "" {
		t.Errorf("no -- means no command: %q", opts.Command)
	}
}

func TestSessionCreateArgsRefusals(t *testing.T) {
	for _, argv := range [][]string{
		{"--"},                // nothing after --
		{"--volume"},          // flag without value
		{"--bogus"},           // unknown flag
		{"stray"},             // unexpected positional
		{"--gpus", "0", "--"}, // -- at the end
	} {
		if _, err := parseSessionCreateArgs("h", argv); err == nil {
			t.Errorf("accepted %v", argv)
		}
	}
}

func TestSessionCreateArgsSingleBareWord(t *testing.T) {
	opts, err := parseSessionCreateArgs("h", []string{"--cwd", "/x", "--", "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Command != "'ls'" || opts.RemoteCWD != "/x" || opts.Container != "" {
		t.Errorf("got %+v", opts)
	}
}
