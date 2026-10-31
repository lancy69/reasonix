//go:build !windows

package config

import "path/filepath"

var evalSymlinks = filepath.EvalSymlinks
