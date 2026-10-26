package main

import "testing"

func TestTaskManagerLaunchIsolatesAppendedArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--task-manager", "--", `C:\Windows\System32\taskmgr.exe`, "/7"},
		{"--task-manager", "--", `C:\Windows\System32\taskmgr.exe`, "--ryzen-probe", "must-not-write.txt", "--version", "--tab=settings"},
		{"--task-manager", "--ryzen-probe", "must-not-write.txt", "--version", "--tab=settings"},
		{"--tab", "disk", "--task-manager", "--", `C:\Windows\System32\taskmgr.exe`},
	} {
		opts, err := parseLaunchOptions(args)
		if err != nil {
			t.Errorf("Task Manager arguments %q rejected: %v", args, err)
			continue
		}
		if !opts.taskManager || opts.tab != "processes" || opts.probe != "" || opts.showVersion {
			t.Errorf("Task Manager arguments %q escaped launch mode: %+v", args, opts)
		}
	}
}

func TestOrdinaryLaunchOptionsRemainAvailable(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want launchOptions
	}{
		{[]string{"--version"}, launchOptions{showVersion: true}},
		{[]string{"--tab", "disk"}, launchOptions{tab: "disk"}},
		{[]string{"--ryzen-probe", "diagnostic.txt"}, launchOptions{probe: "diagnostic.txt"}},
		{nil, launchOptions{}},
	} {
		got, err := parseLaunchOptions(tc.args)
		if err != nil || got != tc.want {
			t.Errorf("parse %q = %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}
}
