package core

import (
	"context"
	"io"
	"strings"
	"testing"
)

func imageSpec() *ContainerSpec {
	return &ContainerSpec{
		Name:  "hammer",
		Image: "ghcr.io/dostos/hammer:v1",
		User:  "worker",
		Home:  &HomeSpec{Volume: "relay-home", Target: "/relay/home"},
		Env:   []string{"ANTHROPIC_API_KEY"},
		GPU:   "none",
	}
}

func TestImageBackedIsExactlyImageWithoutDevcontainer(t *testing.T) {
	if !imageSpec().ImageBacked() {
		t.Fatal("image: without devcontainer: must be image-backed")
	}
	dev := devSpec()
	dev.Image = "something"
	if dev.ImageBacked() {
		t.Error("a devcontainer spec is not image-backed even if image: is set")
	}
	plain := &ContainerSpec{Name: "x", Container: "existing"}
	if plain.ImageBacked() {
		t.Error("a container:-only spec is not image-backed")
	}
}

func TestInstanceNaming(t *testing.T) {
	s := imageSpec()
	cases := map[string]string{"": "hammer", "hammer": "hammer", "order-42": "hammer-order-42"}
	for session, want := range cases {
		if got := s.InstanceName(session); got != want {
			t.Errorf("InstanceName(%q) = %q, want %q", session, got, want)
		}
	}
}

func TestImageUpCommandRendersRunArgs(t *testing.T) {
	inst := ImageInstance{
		Spec:    imageSpec(),
		Name:    "hammer-order-42",
		Volumes: []string{"ev-order-42:/out", "inputs:/in:ro"},
		Binds:   []string{"/data/corpus:/corpus:ro"},
		GPUs:    "0,1",
		Network: "host",
	}
	args, err := imageRunArgs(inst)
	if err != nil {
		t.Fatal(err)
	}
	run := strings.Join(args, " ")
	// Every value is single-quoted for the host shell; flags and the env
	// NAME are bare words.
	for _, want := range []string{
		"run -d",
		"--name 'hammer-order-42'",
		"--label 'relay.container=hammer'",
		"--label 'relay.instance=hammer-order-42'",
		`--gpus '"device=0,1"'`,
		"--network 'host'",
		"-u 'worker'",
		"-v 'relay-home:/relay/home'",
		"-v 'ev-order-42:/out'",
		"-v 'inputs:/in:ro'",
		"-v '/data/corpus:/corpus:ro'",
		"-e ANTHROPIC_API_KEY",
		"'ghcr.io/dostos/hammer:v1' sleep infinity",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("missing %q in:\n%s", want, run)
		}
	}
	// The value of the env var must never appear: only its name is passed.
	if strings.Contains(run, "ANTHROPIC_API_KEY=") {
		t.Errorf("env passed by value:\n%s", run)
	}
	// The full command wraps that run in an idempotent host-shell script.
	cmd, err := ImageUpCommand(inst, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bash -ilc", "docker inspect -f", "echo running", "docker start", "echo started", "docker run -d", "echo created", "sleep infinity"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "rm -f") {
		t.Errorf("a plain up must not remove an existing instance:\n%s", cmd)
	}
}

func TestImageUpRecreateRemovesFirst(t *testing.T) {
	cmd, err := ImageUpCommand(ImageInstance{Spec: imageSpec()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "docker rm -f") {
		t.Errorf("recreate must remove the existing instance first:\n%s", cmd)
	}
	if strings.Index(cmd, "rm -f") > strings.Index(cmd, "docker run") {
		t.Errorf("removal must precede the run:\n%s", cmd)
	}
}

func TestImageUpUsesSpecDefaultsWhenInstanceIsBare(t *testing.T) {
	s := imageSpec()
	s.GPU = "all"
	s.Network = "bridge"
	args, err := imageRunArgs(ImageInstance{Spec: s})
	if err != nil {
		t.Fatal(err)
	}
	run := strings.Join(args, " ")
	if !strings.Contains(run, "--gpus all") || !strings.Contains(run, "--network 'bridge'") {
		t.Errorf("spec defaults not applied:\n%s", run)
	}
	if !strings.Contains(run, "--name 'hammer' ") {
		t.Errorf("bare instance should be named after the spec:\n%s", run)
	}
}

func TestImageUpRejectsBadInput(t *testing.T) {
	bad := []ImageInstance{
		{Spec: devSpec()},                                    // not image-backed
		{Spec: imageSpec(), GPUs: "gpu0"},                    // not an index
		{Spec: imageSpec(), GPUs: "0,,1"},                    // empty device
		{Spec: imageSpec(), Volumes: []string{"/host:/in"}},  // host path as a volume
		{Spec: imageSpec(), Binds: []string{"rel/path:/in"}}, // relative host path
		{Spec: imageSpec(), Binds: []string{"/abs:/in:zz"}},  // bad mode
		{Spec: imageSpec(), Volumes: []string{"v:/a:/b"}},    // colon in target
	}
	for i, inst := range bad {
		if _, err := ImageUpCommand(inst, false); err == nil {
			t.Errorf("case %d accepted: %+v", i, inst)
		}
	}
	envSpec := imageSpec()
	envSpec.Env = []string{"BAD NAME"}
	if _, err := ImageUpCommand(ImageInstance{Spec: envSpec}, false); err == nil {
		t.Error("an env name that is not a shell identifier must be refused")
	}
}

func TestParseVolumeMountReadOnly(t *testing.T) {
	m, err := ParseVolumeMount("inputs:/in:ro")
	if err != nil {
		t.Fatal(err)
	}
	if m.Volume != "inputs" || m.Target != "/in" || !m.ReadOnly {
		t.Errorf("got %+v", m)
	}
	rw, err := ParseVolumeMount("ev:/out")
	if err != nil {
		t.Fatal(err)
	}
	if rw.ReadOnly {
		t.Errorf("no suffix must mean read-write: %+v", rw)
	}
}

func TestReadOnlyVolumeReachesTheDevcontainerMount(t *testing.T) {
	s := devSpec()
	s.Volumes = append(s.Volumes, "inputs:/in:ro")
	cmd, err := DevcontainerUpCommand(s, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "type=volume,source=inputs,target=/in,readonly") {
		t.Errorf("readonly not rendered:\n%s", cmd)
	}
}

func TestParseBindMount(t *testing.T) {
	b, err := ParseBindMount("/data/corpus:/corpus:ro")
	if err != nil {
		t.Fatal(err)
	}
	if b.Host != "/data/corpus" || b.Target != "/corpus" || !b.ReadOnly {
		t.Errorf("got %+v", b)
	}
	for _, bad := range []string{"data:/corpus", "/data:corpus", "/data", "/a:/b:ro:x"} {
		if _, err := ParseBindMount(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestImageTeardownLeavesTheVolumes(t *testing.T) {
	down := ImageDownCommand(imageSpec(), "hammer-order-42")
	for _, forbidden := range []string{"volume rm", "volume prune", " -v", "--volumes"} {
		if strings.Contains(down, forbidden) {
			t.Errorf("teardown must not touch volumes, found %q in: %s", forbidden, down)
		}
	}
	if !strings.Contains(down, "rm -f 'hammer-order-42'") {
		t.Errorf("teardown should remove exactly the instance: %s", down)
	}
}

func TestParseImageStatus(t *testing.T) {
	running, code, id, present := parseImageStatus("true 0 abc123\n")
	if !running || code != 0 || id != "abc123" || !present {
		t.Errorf("running: %v %d %q %v", running, code, id, present)
	}
	running, code, _, present = parseImageStatus("false 137 def456")
	if running || code != 137 || !present {
		t.Errorf("exited: %v %d %v", running, code, present)
	}
	if _, _, _, present = parseImageStatus("absent"); present {
		t.Error("absent must not be present")
	}
}

// scriptedTransport answers each Run with the next scripted output and
// records every command it saw.
type scriptedTransport struct {
	outputs []string
	seen    []string
}

func (s *scriptedTransport) ID() string { return "scripted" }
func (s *scriptedTransport) Run(_ context.Context, _, command string) (string, string, error) {
	s.seen = append(s.seen, command)
	if len(s.outputs) == 0 {
		return "", "", nil
	}
	out := s.outputs[0]
	s.outputs = s.outputs[1:]
	return out, "", nil
}
func (s *scriptedTransport) RunStream(context.Context, string, string, io.Writer) error { return nil }
func (s *scriptedTransport) ReadFile(context.Context, string) ([]byte, error)           { return nil, io.EOF }
func (s *scriptedTransport) WriteFile(context.Context, string, []byte, string) error    { return nil }
func (s *scriptedTransport) Interactive(context.Context, string) error                  { return nil }
func (s *scriptedTransport) InteractiveCommand(remoteCmd string) string                 { return remoteCmd }

func TestImageUpPreparesVolumesForTheExecUser(t *testing.T) {
	tr := &scriptedTransport{outputs: []string{"created\nabc123\n", "prepared\n"}}
	st, err := ImageUp(context.Background(), tr, "hamburg", ImageInstance{Spec: imageSpec(), Name: "hammer-o1"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.ContainerID != "abc123" || st.Instance != "hammer-o1" || !st.Running || !st.Present || st.Detail != "created" {
		t.Errorf("status: %+v", st)
	}
	if len(tr.seen) != 2 {
		t.Fatalf("expected up + prepare, saw %d commands: %v", len(tr.seen), tr.seen)
	}
	// The chown runs inside a nested quote layer (docker exec … bash -lc '…'),
	// so the user name appears escaped; the root exec flag is at the outer layer.
	if !strings.Contains(tr.seen[1], "chown -R") || !strings.Contains(tr.seen[1], "worker") || !strings.Contains(tr.seen[1], "-u '0'") {
		t.Errorf("prepare must chown the volumes as root for the exec user: %s", tr.seen[1])
	}
}

func TestSessionExtrasRefusedOffImageBacked(t *testing.T) {
	profile := &HostProfile{Containers: []ContainerSpec{*devSpec()}}
	opts := CreateOpts{HostID: "h", Container: devSpec().Name, ContainerVolumes: []string{"x:/y"}}
	if _, err := resolveSessionContainer(context.Background(), &scriptedTransport{}, profile, opts); err == nil ||
		!strings.Contains(err.Error(), "not image-backed") {
		t.Errorf("expected a refusal naming the reason, got %v", err)
	}
}

func TestSessionOnImageBackedSpecBringsItsInstanceUp(t *testing.T) {
	profile := &HostProfile{Containers: []ContainerSpec{*imageSpec()}}
	tr := &scriptedTransport{outputs: []string{"created\nabc123\n", "prepared\n"}}
	opts := CreateOpts{HostID: "hamburg", Container: "hammer", Name: "order-42", ContainerEphemeral: true,
		ContainerVolumes: []string{"ev:/out"}, ContainerGPUs: "3"}
	ref, err := resolveSessionContainer(context.Background(), tr, profile, opts)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Ref != "hammer-order-42" || ref.SpecName != "hammer" || !ref.Ephemeral || ref.User != "worker" || ref.Home != "/relay/home" {
		t.Errorf("ref: %+v", ref)
	}
	// seen[0] is the host-shell-wrapped up command, so inner quotes are
	// escaped; the bare substrings are what survive either way.
	if len(tr.seen) == 0 || !strings.Contains(tr.seen[0], "hammer-order-42") || !strings.Contains(tr.seen[0], "ev:/out") ||
		!strings.Contains(tr.seen[0], "device=3") {
		t.Errorf("the session's extras must reach the run: %v", tr.seen)
	}
}

func TestInstanceForRefusesANameOffImageBacked(t *testing.T) {
	// The three operations that take --name route through one helper, so one
	// test covers the refusal for all of them.
	for _, spec := range []*ContainerSpec{devSpec(), {Name: "plain", Container: "existing"}} {
		if _, err := instanceFor(spec, "order-42"); err == nil || !strings.Contains(err.Error(), "not image-backed") {
			t.Errorf("%s: expected a refusal naming the reason, got %v", spec.Name, err)
		}
		if got, err := instanceFor(spec, ""); err != nil || got != spec.Container {
			t.Errorf("%s: bare instance should be the spec's container %q, got %q (%v)", spec.Name, spec.Container, got, err)
		}
	}
	if got, err := instanceFor(imageSpec(), "order-42"); err != nil || got != "hammer-order-42" {
		t.Errorf("image-backed: got %q (%v)", got, err)
	}
}

func TestPlainContainerStateTellsAbsentFromStopped(t *testing.T) {
	cases := map[string][2]bool{"true\n": {true, true}, "false": {false, true}, "absent": {false, false}, "": {false, false}}
	for out, want := range cases {
		running, present := parsePlainContainerState(out)
		if running != want[0] || present != want[1] {
			t.Errorf("%q: got running=%v present=%v, want %v", out, running, present, want)
		}
	}
}

func TestImageUpSaysWhenExtrasWereNotReapplied(t *testing.T) {
	tr := &scriptedTransport{outputs: []string{"running\nabc123\n", "prepared\n"}}
	inst := ImageInstance{Spec: imageSpec(), Name: "hammer-o1", GPUs: "2,3"}
	st, err := ImageUp(context.Background(), tr, "hamburg", inst, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Detail, "not reapplied") {
		t.Errorf("a reused instance must say the extras were ignored: %q", st.Detail)
	}
	// Without extras a reuse is just a reuse.
	tr = &scriptedTransport{outputs: []string{"running\nabc123\n", "prepared\n"}}
	st, err = ImageUp(context.Background(), tr, "hamburg", ImageInstance{Spec: imageSpec(), Name: "hammer-o1"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Detail != "running" {
		t.Errorf("no extras, no warning: %q", st.Detail)
	}
}
