//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gotm/internal/taskmanager"
)

func (l layout) routingStore() taskmanager.Store {
	if l.taskManagerStore != nil {
		return *l.taskManagerStore
	}
	return taskmanager.MachineStore()
}

// Both operations require the installed helper and the same pinned path/ACL
// policy used while installing. A copied setup executable cannot enable IFEO.
func (l layout) maintainTaskManager(self, mode string) error {
	if mode != "on" && mode != "off" {
		return fmt.Errorf("invalid Task Manager maintenance mode %q", mode)
	}
	self, err := normalizeDestinationPath(self)
	if err != nil {
		return err
	}
	_, installedDir := l.installed()
	if installedDir == "" || !strings.EqualFold(self, filepath.Join(installedDir, "uninstall.exe")) {
		return fmt.Errorf("Task Manager replacement requires the registered installed uninstall.exe; install gotm first and run its helper as administrator")
	}
	l.dir = installedDir
	guard, err := l.protectDestination()
	if err != nil {
		return fmt.Errorf("Task Manager replacement requires a protected installation: %w", err)
	}
	defer guard.close()
	requiredFiles := []string{"uninstall.exe"}
	if mode == "on" {
		requiredFiles = append(requiredFiles, "gotm.exe")
	}
	for _, name := range requiredFiles {
		info, err := os.Stat(filepath.Join(l.dir, name))
		if err != nil {
			return fmt.Errorf("incomplete installation (%s): %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("incomplete installation: %s is not a regular file", name)
		}
	}
	exe := filepath.Join(l.dir, "gotm.exe")
	store := l.routingStore()
	if mode == "on" {
		return store.Enable(exe)
	}
	return store.Restore(exe)
}

// The new protected payload must already exist before the route is moved.
// On registration failure, undo only our exact update. If a different helper
// changed the value concurrently, preserve it and retain both payloads.
func (l layout) commitInstall(previousDir string, register func() error) error {
	store := l.routingStore()
	oldExe, newExe := filepath.Join(previousDir, "gotm.exe"), filepath.Join(l.dir, "gotm.exe")
	moved := false
	if previousDir != "" && !strings.EqualFold(previousDir, l.dir) {
		var err error
		moved, err = store.Retarget(oldExe, newExe)
		if err != nil {
			return fmt.Errorf("update Task Manager replacement: %w", err)
		}
	}
	if err := register(); err != nil {
		if moved {
			_, rollbackErr := store.Retarget(newExe, oldExe)
			if rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("restore previous Task Manager replacement (payloads retained): %w", rollbackErr))
			}
		}
		return err
	}
	return nil
}
