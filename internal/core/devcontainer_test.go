package core

import (
	"strings"
	"testing"
)

func devSpec() *ContainerSpec {
	return &ContainerSpec{
		Name:      "oqb",
		User:      "1005",
		Container: "",
		Devcontainer: &DevcontainerSpec{
			WorkspaceFolder: "~/gh/opaquebench",
			GPU:             "none",
		},
		Toolkit: &ToolkitSpec{
			Volume:    "relay-toolkit",
			Target:    "/relay/toolkit",
			Provision: "npm i -g --prefix /relay/toolkit @anthropic-ai/claude-code",
		},
		Home:    &HomeSpec{Volume: "relay-home", Target: "/relay/home"},
		Volumes: []string{"relay-pip:/root/.cache/pip"},
		Env:     []string{"ANTHROPIC_API_KEY"},
	}
}

func TestDevcontainerUpCommand(t *testing.T) {
	got, err := DevcontainerUpCommand(devSpec(), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "bash -ilc ") {
		// The devcontainer CLI is only on an interactive shell's PATH.
		t.Errorf("up must run under an interactive login shell:\n%s", got)
	}
	for _, want := range []string{
		"devcontainer up",
		"relay.container=oqb",
		"--gpu-availability none",
		"type=volume,source=relay-toolkit,target=/relay/toolkit",
		"type=volume,source=relay-home,target=/relay/home",
		"type=volume,source=relay-pip,target=/root/.cache/pip",
		`ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY`,
		"--log-format json",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--remove-existing-container") {
		t.Errorf("did not ask to recreate, but got the flag:\n%s", got)
	}
	// The workspace folder must be tilde-expanded by the remote shell, not
	// passed as a literal ~ that docker would create as a directory.
	if !strings.Contains(got, "gh/opaquebench") {
		t.Errorf("workspace folder missing:\n%s", got)
	}
}

func TestDevcontainerUpRecreate(t *testing.T) {
	got, err := DevcontainerUpCommand(devSpec(), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "--remove-existing-container") {
		t.Errorf("recreate flag missing:\n%s", got)
	}
}

func TestDevcontainerUpRejectsBadInput(t *testing.T) {
	noWF := devSpec()
	noWF.Devcontainer.WorkspaceFolder = ""
	if _, err := DevcontainerUpCommand(noWF, false); err == nil {
		t.Error("expected an error for a missing workspace_folder")
	}
	badGPU := devSpec()
	badGPU.Devcontainer.GPU = "some"
	if _, err := DevcontainerUpCommand(badGPU, false); err == nil {
		t.Error("expected an error for an invalid gpu availability")
	}
	// An env NAME is interpolated into a host shell word, so anything that is
	// not a shell identifier must be refused rather than escaped.
	badEnv := devSpec()
	badEnv.Env = []string{"A; rm -rf /"}
	if _, err := DevcontainerUpCommand(badEnv, false); err == nil {
		t.Error("expected an error for a non-identifier env name")
	}
	plain := &ContainerSpec{Name: "plain", Container: "x"}
	if _, err := DevcontainerUpCommand(plain, false); err == nil {
		t.Error("expected an error for a container with no devcontainer block")
	}
}

func TestParseVolumeMount(t *testing.T) {
	vm, err := ParseVolumeMount("relay-home:/relay/home")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vm.Volume != "relay-home" || vm.Target != "/relay/home" {
		t.Errorf("got %+v", vm)
	}
	// A host path as the source would be a bind mount, which docker creates on
	// the host if absent — that belongs in expose:, and is refused here.
	for _, bad := range []string{"/host/path:/in", "~/x:/in", "novolumesep", "relay-home:relative", "relay-home:"} {
		if _, err := ParseVolumeMount(bad); err == nil {
			t.Errorf("expected %q to be refused", bad)
		}
	}
}

func TestRefForCarriesToolkitAndHome(t *testing.T) {
	ref := devSpec().RefFor("abc123", "/workspaces/oqb")
	if ref.Ref != "abc123" || ref.CWD != "/workspaces/oqb" || ref.User != "1005" {
		t.Fatalf("got %+v", ref)
	}
	if ref.Home != "/relay/home" {
		t.Errorf("home volume not carried as $HOME: %+v", ref)
	}
	if ref.PathPrepend != "/relay/toolkit/bin" {
		t.Errorf("toolkit bin dir not derived: %+v", ref)
	}
}

func TestContainerExecPathPrependSurvivesExec(t *testing.T) {
	ref := devSpec().RefFor("abc123", "/workspaces/oqb")
	// With a tty the inner command is the argument of `exec`; the PATH export
	// must therefore precede the whole composed script, never sit between
	// `exec` and the command.
	got, err := ContainerExec("docker", ref, "claude --version", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "exec export") {
		t.Errorf("PATH export landed after exec:\n%s", got)
	}
	if !strings.Contains(got, "export PATH=") || !strings.Contains(got, "/relay/toolkit/bin") {
		t.Errorf("PATH export missing:\n%s", got)
	}
	if !strings.Contains(got, "-e 'HOME=/relay/home'") {
		t.Errorf("HOME not set from the home volume:\n%s", got)
	}
	if !strings.Contains(got, "-u '1005'") {
		t.Errorf("exec user missing:\n%s", got)
	}
	idxExport := strings.Index(got, "export PATH=")
	idxCd := strings.Index(got, "cd ")
	if idxExport < 0 || idxCd < 0 || idxExport > idxCd {
		t.Errorf("expected the PATH export before the cd:\n%s", got)
	}
}

func TestToolkitProvisionIsGuardedAndIdempotent(t *testing.T) {
	spec := devSpec()
	ref := spec.RefFor("abc123", "")
	got, err := ToolkitProvisionCommand(spec, ref, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "/relay/toolkit/.relay-toolkit-ok") {
		t.Errorf("stamp guard missing:\n%s", got)
	}
	if !strings.Contains(got, "already-provisioned") {
		t.Errorf("expected a short-circuit when the stamp exists:\n%s", got)
	}
	forced, err := ToolkitProvisionCommand(spec, ref, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(forced, "already-provisioned") {
		t.Errorf("--reprovision must not short-circuit:\n%s", forced)
	}
	none := devSpec()
	none.Toolkit = nil
	if _, err := ToolkitProvisionCommand(none, ref, false); err == nil {
		t.Error("expected an error when no toolkit.provision is declared")
	}
}

func TestResolveAndDownUseTheIDLabel(t *testing.T) {
	spec := devSpec()
	resolve := DevcontainerResolveCommand(spec)
	if !strings.Contains(resolve, "label=relay.container=oqb") || !strings.Contains(resolve, "status=running") {
		t.Errorf("resolve does not scope to the running labelled container: %s", resolve)
	}
	down := DevcontainerDownCommand(spec)
	if !strings.Contains(down, "label=relay.container=oqb") {
		t.Errorf("down is not scoped to the id label: %s", down)
	}
	if strings.Contains(down, "volume rm") {
		t.Errorf("down must not remove the named volumes: %s", down)
	}
	custom := devSpec()
	custom.Devcontainer.IDLabel = "team=beholder"
	if !strings.Contains(DevcontainerResolveCommand(custom), "label=team=beholder") {
		t.Error("a declared id_label must win over the default")
	}
}

func TestParseDevcontainerOutcome(t *testing.T) {
	out := `[1234 ms] Start: Run: docker ps
some progress noise
{"outcome":"success","containerId":"deadbeef","remoteUser":"vscode","remoteWorkspaceFolder":"/workspaces/oqb"}`
	o, ok := parseDevcontainerOutcome(out)
	if !ok || o.Outcome != "success" || o.ContainerID != "deadbeef" {
		t.Fatalf("got %+v ok=%v", o, ok)
	}
	fail := `{"outcome":"error","message":"boom","description":"could not build"}`
	o, ok = parseDevcontainerOutcome(fail)
	if !ok || o.Outcome != "error" || o.Description != "could not build" {
		t.Fatalf("got %+v ok=%v", o, ok)
	}
	if _, ok := parseDevcontainerOutcome("no json here"); ok {
		t.Error("expected no outcome from plain text")
	}
}

func TestVolumeMountsOrderAndValidation(t *testing.T) {
	got, err := devSpec().VolumeMounts()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 || got[0].Volume != "relay-toolkit" || got[1].Volume != "relay-home" {
		t.Fatalf("unexpected mounts: %+v", got)
	}
	bad := devSpec()
	bad.Toolkit.Target = ""
	if _, err := bad.VolumeMounts(); err == nil {
		t.Error("expected an error when toolkit.volume has no target")
	}
}

func TestMinimalDevcontainerNeedsNoVolumes(t *testing.T) {
	// A devcontainer declared without toolkit/home/volumes must still come up:
	// no mounts, nothing to chown, nothing to provision.
	c := &ContainerSpec{
		Name:         "bare",
		Devcontainer: &DevcontainerSpec{WorkspaceFolder: "/srv/repo"},
	}
	got, err := DevcontainerUpCommand(c, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "--mount") {
		t.Errorf("no volumes declared, but a mount was emitted:\n%s", got)
	}
	prep, err := ContainerPrepareCommand(c, "cid", "node")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prep != "" {
		t.Errorf("nothing to prepare, but got: %s", prep)
	}
	// Root needs no chown even when volumes exist.
	rootPrep, err := ContainerPrepareCommand(devSpec(), "cid", "root")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rootPrep != "" {
		t.Errorf("root exec needs no chown, but got: %s", rootPrep)
	}
	ref := c.RefFor("cid", "")
	if ref.PathPrepend != "" || ref.Home != "" {
		t.Errorf("bare spec should carry no toolkit/home: %+v", ref)
	}
}

func TestParseRemoteUserPrefersTheLastEntry(t *testing.T) {
	raw := `[{"id":"common-utils"},{"remoteUser":"vscode"},{"remoteUser":"node"},{"overrideCommand":true}]`
	if got := ParseRemoteUser(raw); got != "node" {
		t.Errorf("expected the last remoteUser to win, got %q", got)
	}
	if got := ParseRemoteUser(`[{"id":"x"}]`); got != "" {
		t.Errorf("expected no user, got %q", got)
	}
	if got := ParseRemoteUser("not json"); got != "" {
		t.Errorf("expected no user from junk, got %q", got)
	}
}

func TestSessionContainerRefCarriesLifetimeAndIdentity(t *testing.T) {
	spec := devSpec()
	ref := spec.RefFor("cid123", "/workspaces/oqb")
	ref.SpecName = spec.Name
	ref.Ephemeral = true
	if ref.SpecName != "oqb" || !ref.Ephemeral {
		t.Fatalf("lifetime/identity not carried: %+v", ref)
	}
	// A non-ephemeral ref must never be reaped, which is what keeps a
	// long-lived shared runner alive when one session using it is destroyed.
	keep := spec.RefFor("cid123", "")
	if keep.Ephemeral {
		t.Errorf("a ref must default to non-ephemeral: %+v", keep)
	}
}

func TestEphemeralTeardownLeavesTheVolumes(t *testing.T) {
	// The whole point of the named volumes is that they outlive the container.
	// A teardown that removed them would throw away the agent installs and the
	// logins on every session close.
	down := DevcontainerDownCommand(devSpec())
	for _, forbidden := range []string{"volume rm", "volume prune", "-v", "--volumes"} {
		if strings.Contains(down, forbidden) {
			t.Errorf("teardown must not touch volumes, found %q in: %s", forbidden, down)
		}
	}
	if !strings.Contains(down, "rm -f") {
		t.Errorf("teardown should remove the container: %s", down)
	}
}
