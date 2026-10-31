package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"reasonix/internal/base/fileutil"
	"reasonix/internal/base/testenv"
)

func followupError(err error) any {
	if err == nil {
		return nil
	}
	var pe *os.PathError
	row := map[string]any{"type": fmt.Sprintf("%T", err), "text": err.Error(), "not_exist": os.IsNotExist(err), "permission": os.IsPermission(err)}
	if errors.As(err, &pe) {
		row["op"], row["path"], row["cause_type"] = pe.Op, pe.Path, fmt.Sprintf("%T", pe.Err)
	}
	return row
}

func followupEmit(t *testing.T, row map[string]any) {
	t.Helper()
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CONFIG_HANDLE_FOLLOWUP %s", data)
}

func followupWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func followupConsume(path string) map[string]any {
	info, statErr := os.Stat(path)
	row := map[string]any{"path": path, "stat_error": followupError(statErr)}
	if statErr == nil {
		row["regular"], row["size"] = info.Mode().IsRegular(), info.Size()
	}
	file, openErr := os.Open(path)
	row["open_error"] = followupError(openErr)
	if openErr == nil {
		data, readErr := io.ReadAll(io.LimitReader(file, 4096))
		row["open_read_error"], row["open_contents"], row["close_error"] = followupError(readErr), string(data), followupError(file.Close())
	}
	data, readErr := os.ReadFile(path)
	row["read_file_error"], row["read_file_contents"] = followupError(readErr), string(data)
	return row
}

func followupBackend(handle bool) func(string) (string, error) {
	if !handle {
		return filepath.EvalSymlinks
	}
	return func(path string) (string, error) {
		resolved, err := fileutil.ResolveExistingPath(path)
		if err != nil {
			return filepath.EvalSymlinks(path)
		}
		return resolved, nil
	}
}

func followupLoad(t *testing.T, name, home, root string, expected int) {
	t.Helper()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("REASONIX_CREDENTIALS_STORE", "file")
	for _, handle := range []bool{false, true} {
		func() {
			previous := evalSymlinks
			evalSymlinks = followupBackend(handle)
			defer func() { evalSymlinks = previous }()
			roots := RootsForHome(home)
			cfg, err := roots.LoadForRootReadOnly(root)
			row := map[string]any{"case": name, "handle_backend": handle, "home_argument": home, "user_path": roots.UserConfigPath(), "project_root": root, "expected_recall": expected, "error": followupError(err), "config_returned": cfg != nil}
			if cfg != nil {
				row["recall"], row["matches_expected"], row["warnings"], row["ignored"], row["pending_count"] = cfg.Memory.RecallLimit, cfg.Memory.RecallLimit == expected, cfg.LoadWarnings(), cfg.IgnoredProjectSettings(), len(cfg.PendingProjectPrograms())
			}
			followupEmit(t, row)
		}()
	}
}

func followupComparePath(t *testing.T, name, path string) {
	t.Helper()
	original, originalErr := filepath.EvalSymlinks(path)
	handle, handleErr := fileutil.ResolveExistingPath(path)
	row := map[string]any{"case": name, "input": path, "filepath_result": original, "filepath_error": followupError(originalErr), "handle_result": handle, "handle_error": followupError(handleErr), "input_consumption": followupConsume(path)}
	if originalErr == nil {
		row["filepath_consumption"] = followupConsume(original)
	}
	if handleErr == nil {
		row["handle_consumption"] = followupConsume(handle)
	}
	followupEmit(t, row)
}

func TestConfigHandleBackendFollowup(t *testing.T) {
	t.Run("namespace", func(t *testing.T) {
		root, home, outside := testenv.TempDir(t), testenv.TempDir(t), testenv.TempDir(t)
		for index, name := range []string{"literal-dot.", "literal-space "} {
			t.Run(fmt.Sprintf("literal-file-%d", index), func(t *testing.T) {
				target := `\\?\` + filepath.Join(outside, name)
				expected := 997 + index
				if err := os.WriteFile(target, []byte(fmt.Sprintf("[memory]\nrecall_limit=%d\n", expected)), 0o600); err != nil {
					followupEmit(t, map[string]any{"case": "namespace-file-unavailable", "name": name, "error": followupError(err)})
					return
				}
				plain := filepath.Join(outside, name[:len(name)-1])
				followupWrite(t, plain, "[memory]\nrecall_limit=937\n")
				followupComparePath(t, "namespace-file-consumption", target)
				logical := RootsForHome(home).UserConfigPath()
				if err := os.Symlink(target, logical); err != nil {
					followupEmit(t, map[string]any{"case": "namespace-user-symlink-unavailable", "error": followupError(err)})
					return
				}
				defer os.Remove(logical)
				readlink, err := os.Readlink(logical)
				followupEmit(t, map[string]any{"case": "namespace-user-symlink-target", "logical": logical, "target": readlink, "error": followupError(err), "logical_consumption": followupConsume(logical)})
				followupLoad(t, "public-user-symlink-namespace-file", home, root, expected)
			})
		}
		for index, name := range []string{"literal-home-dot.", "literal-home-space "} {
			t.Run(fmt.Sprintf("literal-home-%d", index), func(t *testing.T) {
				literalHome := `\\?\` + filepath.Join(outside, name)
				if err := os.MkdirAll(literalHome, 0o700); err != nil {
					followupEmit(t, map[string]any{"case": "namespace-home-unavailable", "name": name, "error": followupError(err)})
					return
				}
				expected := 1007 + index
				literalConfig := filepath.Join(literalHome, "config.toml")
				followupWrite(t, literalConfig, fmt.Sprintf("[memory]\nrecall_limit=%d\n", expected))
				followupWrite(t, filepath.Join(literalHome, ".env"), "")
				plainHome := filepath.Join(outside, name[:len(name)-1])
				if err := os.MkdirAll(plainHome, 0o700); err != nil {
					t.Fatal(err)
				}
				followupWrite(t, filepath.Join(plainHome, "config.toml"), "[memory]\nrecall_limit=937\n")
				followupWrite(t, filepath.Join(plainHome, ".env"), "")
				followupComparePath(t, "namespace-home-config-consumption", literalConfig)
				followupLoad(t, "public-explicit-namespace-home", literalHome+`\`, root, expected)
			})
		}
	})
	t.Run("non-listable", func(t *testing.T) {
		parent, home, root := testenv.TempDir(t), testenv.TempDir(t), testenv.TempDir(t)
		projectFile, userFile := filepath.Join(parent, "reasonix.toml"), filepath.Join(parent, "config.toml")
		followupWrite(t, projectFile, "[memory]\nrecall_limit=1017\n")
		followupWrite(t, userFile, "[memory]\nrecall_limit=1018\n")
		followupWrite(t, filepath.Join(parent, ".env"), "")
		restore, err := followupProtectAndDenyListing(parent, []string{projectFile, userFile, filepath.Join(parent, ".env")})
		if err != nil {
			followupEmit(t, map[string]any{"case": "non-listable-unavailable", "error": followupError(err)})
			return
		}
		defer func() {
			if err := restore(); err != nil {
				t.Error(err)
			}
		}()
		_, projectReadErr := os.ReadFile(projectFile)
		_, userReadErr := os.ReadFile(userFile)
		dir, dirErr := os.Open(parent)
		if dirErr == nil {
			_ = dir.Close()
		}
		valid := projectReadErr == nil && userReadErr == nil && os.IsPermission(dirErr)
		followupEmit(t, map[string]any{"case": "protected-child-non-listable-validity", "available": valid, "project_read_error": followupError(projectReadErr), "user_read_error": followupError(userReadErr), "directory_read_refused": os.IsPermission(dirErr), "directory_error": followupError(dirErr)})
		if !valid {
			return
		}
		followupComparePath(t, "protected-child-non-listable-consumption", projectFile)
		followupLoad(t, "public-non-listable-project", home, parent, 1017)
		followupLoad(t, "public-non-listable-user", parent, root, 1018)
	})
	t.Run("junction", func(t *testing.T) {
		root, outside := testenv.TempDir(t), testenv.TempDir(t)
		insideTarget, outsideTarget := filepath.Join(root, "inside-target"), filepath.Join(outside, "outside-target")
		for _, target := range []string{insideTarget, outsideTarget} {
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			followupWrite(t, filepath.Join(target, "settings.toml"), "[memory]\nrecall_limit=1027\n")
		}
		for _, scope := range []struct{ name, target string }{{"inside", insideTarget}, {"outside", outsideTarget}} {
			canonical, canonicalErr := fileutil.ResolveExistingPath(scope.target)
			for _, form := range []struct{ name, target string }{{"selected", scope.target}, {"canonical-long", canonical}} {
				if form.target == "" {
					followupEmit(t, map[string]any{"case": "junction-canonical-target-unavailable", "error": followupError(canonicalErr)})
					continue
				}
				junction := filepath.Join(root, "junction-"+scope.name+"-"+form.name)
				output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", junction, form.target).CombinedOutput()
				if err != nil {
					followupEmit(t, map[string]any{"case": "junction-create-unavailable", "scope": scope.name, "form": form.name, "error": followupError(err), "output": string(output)})
					continue
				}
				readlink, readlinkErr := os.Readlink(junction)
				path := filepath.Join(junction, "settings.toml")
				followupEmit(t, map[string]any{"case": "junction-target", "scope": scope.name, "form": form.name, "declared_target": form.target, "readlink": readlink, "readlink_error": followupError(readlinkErr), "direct_consumption": followupConsume(path)})
				followupComparePath(t, "junction-"+scope.name+"-"+form.name, path)
				logical := filepath.Join(root, "reasonix.toml")
				if err := os.Symlink(path, logical); err != nil {
					followupEmit(t, map[string]any{"case": "junction-project-link-unavailable", "error": followupError(err)})
					continue
				}
				for _, handle := range []bool{false, true} {
					func() {
						previous := evalSymlinks
						evalSymlinks = followupBackend(handle)
						defer func() { evalSymlinks = previous }()
						resolved, err := resolveConfigAccessPath(logical, false)
						row := map[string]any{"case": "junction-project-guard", "scope": scope.name, "form": form.name, "handle_backend": handle, "logical": logical, "resolved": resolved, "error": followupError(err)}
						if err == nil {
							row["consumption"] = followupConsume(resolved)
						}
						followupEmit(t, row)
					}()
				}
				if err := os.Remove(logical); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	t.Log("CONFIG_HANDLE_FOLLOWUP_SCOPE constructed filesystem states under test-user authority; no observed production incident, UNC/cloud/case-sensitive directories/volume-GUID aliases not measured")
}

type followupSavedACL struct {
	path        string
	descriptor  *windows.SECURITY_DESCRIPTOR
	acl         *windows.ACL
	information windows.SECURITY_INFORMATION
}

func followupSaveACL(path string) (followupSavedACL, error) {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return followupSavedACL{}, err
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return followupSavedACL{}, err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return followupSavedACL{}, err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	return followupSavedACL{path, descriptor, acl, info}, nil
}

func followupSetACL(path, sddl string) error {
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

func followupProtectAndDenyListing(parent string, children []string) (func() error, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sid := user.User.Sid.String()
	saved := []followupSavedACL{}
	for _, path := range append(append([]string{}, children...), parent) {
		state, err := followupSaveACL(path)
		if err != nil {
			return nil, err
		}
		saved = append(saved, state)
	}
	restore := func() error {
		var problems []error
		for i := len(saved) - 1; i >= 0; i-- {
			state := saved[i]
			problems = append(problems, windows.SetNamedSecurityInfo(state.path, windows.SE_FILE_OBJECT, state.information, nil, nil, state.acl, nil))
		}
		return errors.Join(problems...)
	}
	for _, path := range children {
		if err := followupSetACL(path, "D:P(A;;FA;;;"+sid+")"); err != nil {
			return nil, errors.Join(err, restore())
		}
	}
	if err := followupSetACL(parent, "D:P(D;;0x1;;;"+sid+")(A;;FA;;;"+sid+")"); err != nil {
		return nil, errors.Join(err, restore())
	}
	return restore, nil
}
