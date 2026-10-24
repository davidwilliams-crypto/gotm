//go:build windows

package metrics

import (
	"strings"
	"testing"
)

const sampleTasks = `<Tasks>
<!-- \Vendor\Updater -->
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Principals><Principal id="Author"><UserId>S-1-5-18</UserId></Principal></Principals>
  <Settings><Enabled>false</Enabled></Settings>
  <Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers>
  <Actions Context="Author"><Exec><Command>"C:\Program Files\Vendor\up.exe"</Command><Arguments>/silent</Arguments></Exec></Actions>
</Task>
<!-- \BootThing -->
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers><BootTrigger /></Triggers>
  <Actions><ComHandler><ClassId>{ABC}</ClassId></ComHandler></Actions>
</Task>
<!-- \DailyOnly -->
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers><CalendarTrigger><StartBoundary>2025-01-01T09:00:00</StartBoundary></CalendarTrigger></Triggers>
  <Actions><Exec><Command>daily.exe</Command></Exec></Actions>
</Task>
<!-- \TriggerOff -->
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers><LogonTrigger><Enabled>false</Enabled></LogonTrigger></Triggers>
  <Actions><Exec><Command>off.exe</Command></Exec></Actions>
</Task>
</Tasks>`

func TestParseTaskXML(t *testing.T) {
	items := parseTaskXML([]byte(sampleTasks))
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (logon + boot; daily and disabled-trigger skipped): %+v", len(items), items)
	}
	up, boot := items[0], items[1]
	if up.ID != `task|\Vendor\Updater` || up.Name != "Updater" || up.Enabled {
		t.Errorf("updater: %+v", up)
	}
	if up.Command != `"C:\Program Files\Vendor\up.exe" /silent` {
		t.Errorf("command = %q", up.Command)
	}
	if !strings.Contains(strings.ToUpper(up.User), "SYSTEM") {
		t.Errorf("user = %q, want the SYSTEM account name", up.User)
	}
	if boot.Name != "BootThing" || !boot.Enabled || boot.Command != "COM handler {ABC}" {
		t.Errorf("boot task: %+v", boot)
	}
}

func TestLogonTasksOnThisPC(t *testing.T) {
	n := 0
	for _, it := range ListStartup() {
		if it.Source == "Scheduled task" {
			n++
		}
	}
	t.Logf("%d logon/boot scheduled tasks found", n)
	if n == 0 {
		t.Error("no scheduled-task startup entries found; every Windows install has some")
	}
}
