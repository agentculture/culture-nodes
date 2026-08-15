package deploytest

import (
	"strings"
	"testing"
)

func TestHumanInboxDeployAdoptsCanonicalUnitsAndRemovesLegacyFiles(t *testing.T) {
	body := humanInboxLaneBody(t, deployScriptText(t))
	for _, unit := range []string{"culture-nodes-human-inbox.service", "culture-nodes-human-inbox-tracker.service"} {
		if !strings.Contains(body, unit) {
			t.Errorf("deploy lane does not install/start canonical %s", unit)
		}
	}
	for _, unit := range []string{"human-inbox-bridge.service", "human-inbox-tracker.service"} {
		for _, action := range []string{"stop", "disable"} {
			if !strings.Contains(body, "systemctl --user "+action+" "+unit) {
				t.Errorf("deploy lane does not %s stale %s", action, unit)
			}
		}
		if !strings.Contains(body, "rm -f ~/.config/systemd/user/"+unit) {
			t.Errorf("deploy lane does not remove stale %s file", unit)
		}
	}
}

func TestHumanInboxPortConflictNamesConflictingUnitAndFails(t *testing.T) {
	script := deployScriptText(t)
	body := humanInboxLaneBody(t, script)
	if !strings.Contains(body, "report_port_conflict") {
		t.Error("human-inbox lane does not invoke the port-conflict reporter")
	}
	if !strings.Contains(script, "Address already in use") || !strings.Contains(script, "conflicting unit") {
		t.Error("human-inbox lane has no explicit port-conflict failure naming the conflicting unit")
	}
	if !strings.Contains(script, "exit 1") {
		t.Error("human-inbox port conflict is not a hard deploy failure")
	}
}
