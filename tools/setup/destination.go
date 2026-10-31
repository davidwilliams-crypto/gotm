//go:build windows

package main

import (
	"gotm/internal/installpath"

	"golang.org/x/sys/windows"
)

func normalizeDestinationPath(path string) (string, error) {
	return installpath.Normalize(path)
}

// normalizeInstallDestination is the strict form used for a destination the
// user supplies at install time (/D=): it also rejects 8.3 short spellings,
// which Normalize must keep accepting when resolving an existing install.
func normalizeInstallDestination(path string) (string, error) {
	return installpath.NormalizeInstall(path)
}

type destinationGuard struct{ *installpath.Guard }

func (g *destinationGuard) close() {
	if g != nil {
		g.Guard.Close()
	}
}

type destinationVolume struct {
	driveType                                         uint32
	driveDevice, volumeDevice, mountVolume, finalRoot string
}

func fromSharedVolume(v installpath.Volume) destinationVolume {
	return destinationVolume{v.DriveType, v.DriveDevice, v.VolumeDevice, v.MountVolume, v.FinalRoot}
}

func (v destinationVolume) shared() installpath.Volume {
	return installpath.Volume{DriveType: v.driveType, DriveDevice: v.driveDevice, VolumeDevice: v.volumeDevice, MountVolume: v.mountVolume, FinalRoot: v.finalRoot}
}

func checkDestinationVolume(v destinationVolume) error {
	return installpath.CheckVolume(v.shared())
}

func queryDestinationVolume(root string, h windows.Handle) (destinationVolume, error) {
	v, err := installpath.QueryVolume(root, h)
	return fromSharedVolume(v), err
}

func inspectDestinationSecurity(h windows.Handle, target bool) error {
	return installpath.InspectSecurity(h, target)
}

func checkDestinationSecurity(sd *windows.SECURITY_DESCRIPTOR, target bool) error {
	return installpath.CheckSecurity(sd, target)
}

func (l layout) protectDestination() (*destinationGuard, error) {
	policy := installpath.Policy{InspectSecurity: l.inspectSecurity}
	if l.queryVolume != nil {
		policy.QueryVolume = func(root string, h windows.Handle) (installpath.Volume, error) {
			v, err := l.queryVolume(root, h)
			return v.shared(), err
		}
	}
	g, err := installpath.Protect(l.dir, ownedFiles, true, policy)
	if err != nil {
		return nil, err
	}
	return &destinationGuard{g}, nil
}
