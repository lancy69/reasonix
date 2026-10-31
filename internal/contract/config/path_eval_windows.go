//go:build windows

package config

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"reasonix/internal/base/fileutil"
)

var evalSymlinks = evalConfigSymlinks

// Only unchanged identities on ordinary local DOS volumes use the handle path.
// Aliases, mounted folders and unavailable metadata retain SDK resolution.
func evalConfigSymlinks(path string) (string, error) {
	if !ordinaryWindowsConfigPath(path) {
		return filepath.EvalSymlinks(path)
	}
	root := [4]uint16{uint16(path[0]), ':', '\\', 0}
	if windows.GetDriveType(&root[0]) != windows.DRIVE_FIXED {
		return filepath.EvalSymlinks(path)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return filepath.EvalSymlinks(path)
	}
	var volume [4]uint16
	if err := windows.GetVolumePathName(name, &volume[0], uint32(len(volume))); err != nil || volume != root {
		return filepath.EvalSymlinks(path)
	}
	resolved, err := fileutil.ResolveExistingPath(path)
	if err != nil || resolved != path {
		return filepath.EvalSymlinks(path)
	}
	return resolved, nil
}

func ordinaryWindowsConfigPath(path string) bool {
	if len(path) < 3 || path[0] < 'A' || path[0] > 'Z' || path[1] != ':' || path[2] != '\\' {
		return false
	}
	if len(path) == 3 {
		return true
	}
	for part := range strings.SplitSeq(path[3:], `\`) {
		if part == "" || strings.ContainsAny(part, "/:~") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
	}
	return true
}
