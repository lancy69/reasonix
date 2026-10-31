package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"reasonix/internal/base/fileutil"
	"reasonix/internal/base/testenv"
)

func TestConfigHandleBackendCompatibility(t *testing.T) {
	root, outside := testenv.TempDir(t), testenv.TempDir(t)
	file := filepath.Join(root, "MixedCaseFile.toml")
	if err := os.WriteFile(file, []byte("[memory]\nrecall_limit=7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deep := root
	for range 12 {
		deep = filepath.Join(deep, "segment")
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	deepFile := filepath.Join(deep, "settings.toml")
	if err := os.WriteFile(deepFile, []byte("[memory]\nrecall_limit=7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emit := func(name, path string) {
		t.Helper()
		original, originalErr := filepath.EvalSymlinks(path)
		handle, handleErr := fileutil.ResolveExistingPath(path)
		same := false
		if originalErr == nil && handleErr == nil {
			a, ea := os.Stat(original)
			b, eb := os.Stat(handle)
			same = ea == nil && eb == nil && os.SameFile(a, b)
		}
		row := map[string]any{"case": name, "input": path,
			"filepath_result": original, "filepath_error": configMechanismError(originalErr),
			"handle_result": handle, "handle_error": configMechanismError(handleErr),
			"same_file": same, "same_string": original == handle}
		previous := evalSymlinks
		evalSymlinks = filepath.EvalSymlinks
		outerOriginal, outerOriginalErr := evalSymlinksAllowMissing(path)
		evalSymlinks = func(p string) (string, error) {
			resolved, err := fileutil.ResolveExistingPath(p)
			if err != nil {
				return filepath.EvalSymlinks(p)
			}
			return resolved, nil
		}
		outerHandle, outerHandleErr := evalSymlinksAllowMissing(path)
		evalSymlinks = previous
		row["outer_original"], row["outer_original_error"] = outerOriginal, configMechanismError(outerOriginalErr)
		row["outer_handle"], row["outer_handle_error"] = outerHandle, configMechanismError(outerHandleErr)
		body, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("CONFIG_HANDLE_COMPATIBILITY %s", body)
	}
	emit("ordinary", file)
	emit("deep", deepFile)
	emit("different-case", strings.ToUpper(file))
	emit("extended-namespace", `\\?\`+file)
	emit("trailing-dot-alias", file+".")
	emit("trailing-space-alias", file+" ")
	for _, name := range []string{"literal-dot.", "literal-space "} {
		path := `\\?\` + filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
			t.Logf("CONFIG_HANDLE_UNAVAILABLE namespace-literal %s: %v", name, err)
		} else {
			emit("namespace-"+name, path)
		}
	}
	long := root
	for range 32 {
		long = filepath.Join(long, "long-segment")
	}
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Logf("CONFIG_HANDLE_UNAVAILABLE extended-length-directory: %v", err)
	} else {
		path := filepath.Join(long, "settings.toml")
		if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
			t.Logf("CONFIG_HANDLE_UNAVAILABLE extended-length-file: %v", err)
		} else {
			emit("long-path", path)
			emit("extended-long-path", `\\?\`+path)
		}
	}
	emit("missing-final", filepath.Join(root, "missing.toml"))
	emit("missing-parent", filepath.Join(root, "missing", "settings.toml"))
	ptr, err := windows.UTF16PtrFromString(file)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, 32768)
	if n, err := windows.GetShortPathName(ptr, &buf[0], uint32(len(buf))); err == nil && n > 0 && n < uint32(len(buf)) {
		short := windows.UTF16ToString(buf[:n])
		emit("short-name-api-result", short)
		if !strings.Contains(short, "~") {
			t.Log("CONFIG_HANDLE_UNAVAILABLE actual 8.3 alias: API returned no tilde short component")
		}
	} else {
		t.Logf("CONFIG_HANDLE_UNAVAILABLE short-name: %v", err)
	}
	for _, item := range []struct{ name, target string }{{"file-symlink", file}, {"broken-symlink", filepath.Join(root, "not-there")}} {
		link := filepath.Join(root, item.name+".toml")
		if err := os.Symlink(item.target, link); err != nil {
			t.Logf("CONFIG_HANDLE_UNAVAILABLE %s: %v", item.name, err)
		} else {
			emit(item.name, link)
		}
	}
	last := file
	chainAvailable := true
	for i := range 70 {
		link := filepath.Join(root, fmt.Sprintf("chain-%03d.toml", i))
		if err := os.Symlink(last, link); err != nil {
			t.Logf("CONFIG_HANDLE_UNAVAILABLE long-symlink-chain at %d: %v", i, err)
			chainAvailable = false
			break
		}
		last = link
	}
	if chainAvailable {
		emit("symlink-chain-70", last)
	}
	junction := filepath.Join(root, "junction")
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", junction, deep).CombinedOutput(); err != nil {
		t.Logf("CONFIG_HANDLE_UNAVAILABLE junction: %v %s", err, output)
	} else {
		emit("junction", filepath.Join(junction, "settings.toml"))
	}
	outsideFile := filepath.Join(outside, "settings.toml")
	if err := os.WriteFile(outsideFile, []byte("[memory]\nrecall_limit=937\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "reasonix.toml")
	if err := os.Symlink(outsideFile, project); err != nil {
		t.Logf("CONFIG_HANDLE_UNAVAILABLE project-escape: %v", err)
	} else {
		for _, handle := range []bool{false, true} {
			previous := evalSymlinks
			if handle {
				evalSymlinks = func(path string) (string, error) {
					resolved, err := fileutil.ResolveExistingPath(path)
					if err != nil {
						return previous(path)
					}
					return resolved, nil
				}
			}
			resolved, err := resolveConfigAccessPath(project, false)
			evalSymlinks = previous
			body, _ := json.Marshal(map[string]any{"case": "project-escape-guard", "handle_backend": handle, "result": resolved, "rejected": err != nil, "error": configMechanismError(err)})
			t.Logf("CONFIG_HANDLE_COMPATIBILITY %s", body)
		}
	}
	permissionDir := filepath.Join(root, "non-listable")
	if err := os.Mkdir(permissionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	permissionFile := filepath.Join(permissionDir, "settings.toml")
	if err := os.WriteFile(permissionFile, []byte("[memory]\nrecall_limit=7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if restore, err := configMechanismDenyListing(permissionDir); err != nil {
		t.Logf("CONFIG_HANDLE_UNAVAILABLE non-listable-DACL: %v", err)
	} else {
		defer func() {
			if err := restore(); err != nil {
				t.Error(err)
			}
		}()
		emit("non-listable-directory", permissionFile)
		_, readErr := os.ReadFile(permissionFile)
		dir, dirErr := os.Open(permissionDir)
		if dirErr == nil {
			_ = dir.Close()
		}
		body, _ := json.Marshal(map[string]any{"case": "non-listable-fixture-validity", "file_readable": readErr == nil, "directory_read_refused": os.IsPermission(dirErr), "read_error": configMechanismError(readErr), "directory_error": configMechanismError(dirErr)})
		t.Logf("CONFIG_HANDLE_COMPATIBILITY %s", body)
	}
	t.Log("CONFIG_HANDLE_UNAVAILABLE UNC/network filesystem and cloud-drive providers: not provisioned; observations do not prove drop-in equivalence")
}

func configMechanismError(err error) any {
	if err == nil {
		return nil
	}
	var pathErr *os.PathError
	row := map[string]any{"type": fmt.Sprintf("%T", err), "text": err.Error(), "not_exist": os.IsNotExist(err), "permission": os.IsPermission(err)}
	if errors.As(err, &pathErr) {
		row["op"], row["path"], row["cause_type"] = pathErr.Op, pathErr.Path, fmt.Sprintf("%T", pathErr.Err)
	}
	return row
}

func configMechanismDenyListing(path string) (func() error, error) {
	original, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	originalACL, _, err := original.DACL()
	if err != nil {
		return nil, err
	}
	control, _, err := original.Control()
	if err != nil {
		return nil, err
	}
	restoreInfo := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		restoreInfo = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sid := user.User.Sid.String()
	restricted, err := windows.SecurityDescriptorFromString("D:P(D;;0x1;;;" + sid + ")(A;;FA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	acl, _, err := restricted.DACL()
	if err != nil {
		return nil, err
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return nil, err
	}
	return func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, restoreInfo, nil, nil, originalACL, nil)
	}, nil
}
