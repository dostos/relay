package homeservice

import (
	"os"
	"strings"
	"testing"
)

func TestServiceAndMigrationArtifactsUsePrimaryBinary(t *testing.T) {
	unit, err := os.ReadFile("../../share/systemd/relay.service")
	if err != nil {
		t.Fatal(err)
	}
	text := string(unit)
	if !strings.Contains(text, "relay service run") || strings.Contains(text, "relayd ") || strings.Contains(text, "relay supervise") {
		t.Fatalf("unified service unit regressed:\n%s", text)
	}
	edge, err := os.ReadFile("../../share/systemd/relay-event.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(edge), "relay service event run") {
		t.Fatalf("edge event unit does not use primary binary:\n%s", edge)
	}
	systemUnit, err := os.ReadFile("../../share/systemd/relay-system.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(systemUnit), "User=REPLACE_USER") || !strings.Contains(string(systemUnit), "relay service run") {
		t.Fatalf("system service template does not preserve the service owner:\n%s", systemUnit)
	}
	installer, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installText := string(installer)
	for _, want := range []string{"RELAY_MIGRATE_SERVICE", "system-owned services preserved", "migration-receipt-", ".relayd.new.", "share/systemd/relay.service", "share/systemd/relay-system.service", "old_installed_build", "command_socket", "process_owners", "proposed_system_unit", "migration-backups", "rollback_backup", "relay.previous"} {
		if !strings.Contains(installText, want) {
			t.Fatalf("installer lacks migration safeguard %q", want)
		}
	}
	if strings.Contains(installText, `./cmd/relayd`) {
		t.Fatal("installer still builds a second daemon executable")
	}
}

func TestCompatibilityRemovalRequiresUnitsAndClientFloor(t *testing.T) {
	raw, err := os.ReadFile("../../docs/unified-service.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "every installed service unit") || !strings.Contains(text, "minimum connected client build") {
		t.Fatal("compatibility removal condition is time-based or incomplete")
	}
}
