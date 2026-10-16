package core

import (
	"strings"
	"testing"
)

func TestHostCapsCommandUsesLoginShell(t *testing.T) {
	got := HostCapsCommand()
	if !strings.HasPrefix(got, "bash -ilc ") {
		t.Fatalf("caps must use the same login-shell PATH as container up:\n%s", got)
	}
	for _, want := range []string{
		"RELAY_CAPS_BEGIN", "RELAY_CAPS_END",
		"command -v docker", "command -v devcontainer",
		"nvidia-smi", "@devcontainers/cli",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestParseHostCaps(t *testing.T) {
	out := `
noise
RELAY_CAPS_BEGIN
DOCKER_PRESENT yes
DOCKER_VERSION Docker version 24.0.2
DOCKER_DAEMON yes
DOCKER_USER yes
DC_PRESENT yes
DC_PATH /home/u/.nvm/versions/node/v20/bin/devcontainer
DC_VERSION 0.75.0
GPU_NVIDIA no
GPU_REASON no nvidia-smi and no nvidia runtime reported by docker info
RELAY_CAPS_END
`
	caps, err := ParseHostCaps("hamburg", out)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.OK || caps.HostID != "hamburg" || caps.Runtime != "docker" {
		t.Fatalf("got %+v", caps)
	}
	if !caps.Docker.Present || !caps.Docker.Daemon || !caps.Docker.UserCanRun || caps.Docker.Version == "" {
		t.Errorf("docker: %+v", caps.Docker)
	}
	if !caps.Devcontainer.Present || caps.Devcontainer.Path == "" || caps.Devcontainer.Version != "0.75.0" {
		t.Errorf("devcontainer: %+v", caps.Devcontainer)
	}
	if caps.GPU.Nvidia || caps.GPU.Reason == "" {
		t.Errorf("gpu: %+v", caps.GPU)
	}
}

func TestParseHostCapsMissingDocker(t *testing.T) {
	out := `RELAY_CAPS_BEGIN
DOCKER_PRESENT no
DOCKER_DAEMON no
DOCKER_USER no
DOCKER_REASON docker not found on PATH; install docker
DC_PRESENT no
DC_REASON install it
GPU_NVIDIA no
RELAY_CAPS_END`
	caps, err := ParseHostCaps("h", out)
	if err != nil {
		t.Fatal(err)
	}
	if caps.OK {
		t.Fatal("missing docker must not be ok")
	}
	if caps.Docker.Reason == "" || caps.Devcontainer.Reason == "" {
		t.Fatalf("reasons required: %+v", caps)
	}
}

func TestParseHostCapsRequiresBlock(t *testing.T) {
	if _, err := ParseHostCaps("h", "no markers"); err == nil {
		t.Fatal("expected error")
	}
}
