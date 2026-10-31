//go:build windows

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"reasonix/internal/base/fileutil"
	"reasonix/internal/base/testenv"
)

func TestWindowsConfigLoadKeepsSDKPathSemantics(t *testing.T) {
	for _, name := range []string{"ordinary", "case", "extended", "literal-space-home", "junction-inside", "junction-outside", "broken-link"} {
		t.Run(name, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(testenv.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			home, root := filepath.Join(base, "home"), filepath.Join(base, "workspace")
			for _, dir := range []string{home, root} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			wantRecall := 37
			switch name {
			case "literal-space-home":
				home = `\\?\` + filepath.Join(base, "literal-home ")
				if err := os.Mkdir(home, 0o700); err != nil {
					t.Fatal(err)
				}
				home += `\`
				t.Cleanup(func() {
					if err := os.RemoveAll(home); err != nil {
						t.Error(err)
					}
				})
			case "junction-inside", "junction-outside":
				target := filepath.Join(root, "source")
				if name == "junction-outside" {
					target = filepath.Join(base, "outside")
				}
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "reasonix.toml"), []byte("[memory]\nrecall_limit = 71\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, ".reasonix"), target).CombinedOutput(); err != nil {
					t.Skipf("junctions unavailable: %v %s", err, out)
				}
			case "broken-link":
				if err := os.Symlink(filepath.Join(base, "missing.toml"), filepath.Join(root, "reasonix.toml")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[memory]\nrecall_limit = 37\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "case":
				original, err := os.Stat(home)
				if err != nil {
					t.Fatal(err)
				}
				alias := strings.ToUpper(home)
				upper, err := os.Stat(alias)
				if err != nil || !os.SameFile(original, upper) {
					t.Skipf("uppercase home is not an alias on this filesystem: %q (%v)", alias, err)
				}
				home = alias
			case "extended":
				home = `\\?\` + home
			}
			t.Setenv("REASONIX_HOME", home)
			t.Setenv("REASONIX_CREDENTIALS_STORE", "file")
			backend := evalSymlinks
			t.Cleanup(func() { evalSymlinks = backend })
			evalSymlinks = filepath.EvalSymlinks
			want, wantErr := RootsForHome(home).LoadForRootReadOnly(root)
			evalSymlinks = backend
			got, gotErr := RootsForHome(home).LoadForRootReadOnly(root)
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("load error = %v, SDK load error = %v", gotErr, wantErr)
			}
			if wantErr != nil {
				if name != "junction-inside" && name != "junction-outside" && name != "broken-link" {
					t.Fatalf("SDK control failed for a supported config path: %v", wantErr)
				}
				var wantPath, gotPath *fs.PathError
				if errors.As(wantErr, &wantPath) && (!errors.As(gotErr, &gotPath) || gotPath.Op != wantPath.Op || gotPath.Path != wantPath.Path || !errors.Is(gotErr, wantPath.Err)) {
					t.Fatalf("load lost the SDK path or typed cause: got %v, SDK %v", gotErr, wantErr)
				}
				return
			}
			if name == "junction-outside" || name == "broken-link" {
				t.Fatal("SDK control accepted an escaping or broken project link")
			}
			if name == "junction-inside" {
				wantRecall = 71
			}
			if want.Memory.RecallLimit != wantRecall || got.Memory.RecallLimit != wantRecall {
				t.Fatalf("recall limit = %d, SDK = %d, want %d", got.Memory.RecallLimit, want.Memory.RecallLimit, wantRecall)
			}
			if len(want.LoadWarnings()) != 0 || len(got.LoadWarnings()) != 0 {
				t.Fatalf("unexpected load warnings: got %v, SDK %v", got.LoadWarnings(), want.LoadWarnings())
			}
		})
	}
}

func TestWindowsConfigLoadObservesSourceReplacement(t *testing.T) {
	base, err := filepath.EvalSymlinks(testenv.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	home, root := filepath.Join(base, "home"), filepath.Join(base, "workspace")
	for _, dir := range []string{home, root} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("REASONIX_CREDENTIALS_STORE", "file")
	userPath, projectPath := filepath.Join(home, "config.toml"), filepath.Join(root, "reasonix.toml")
	write := func(path, body string) {
		t.Helper()
		if err := fileutil.AtomicWriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(recall int, ratio float64) {
		t.Helper()
		cfg, err := RootsForHome(home).LoadForRootReadOnly(root)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Memory.RecallLimit != recall || cfg.Agent.CompactRatio != ratio {
			t.Fatalf("loaded recall/ratio = %d/%v, want %d/%v", cfg.Memory.RecallLimit, cfg.Agent.CompactRatio, recall, ratio)
		}
	}
	write(userPath, "[memory]\nrecall_limit = 17\n[agent]\ncompact_ratio = 0.41\n")
	write(projectPath, "[agent]\ncompact_ratio = 0.61\n")
	check(17, 0.61)
	write(userPath, "[memory]\nrecall_limit = 29\n[agent]\ncompact_ratio = 0.41\n")
	check(29, 0.61)
	if _, err := SaveMinimalProjectCompactRatio(projectPath, 0.72); err != nil {
		t.Fatal(err)
	}
	check(29, 0.72)
	for _, entry := range []struct {
		name   string
		recall int
	}{{"first.toml", 47}, {"second.toml", 53}} {
		target := filepath.Join(base, entry.name)
		write(target, fmt.Sprintf("[memory]\nrecall_limit = %d\n", entry.recall))
		if err := os.Remove(userPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, userPath); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		check(entry.recall, 0.72)
	}
}

func TestWindowsConfigLoadReducesPathResolutionAllocations(t *testing.T) {
	base, err := filepath.EvalSymlinks(testenv.TempDir(t))
	if err != nil {
		t.Skipf("TEMP cannot supply a canonical DOS fixture: %v", err)
	}
	if len(base) < 3 || base[0] < 'A' || base[0] > 'Z' || base[1] != ':' || base[2] != '\\' || filepath.Clean(base) != base || strings.ContainsAny(base[2:], "/:~") {
		t.Skipf("TEMP does not provide an ordinary canonical DOS path: %q", base)
	}
	for part := range strings.SplitSeq(base[3:], `\`) {
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			t.Skipf("TEMP has a namespace-sensitive component: %q", base)
		}
	}
	drive := [4]uint16{uint16(base[0]), ':', '\\', 0}
	if windows.GetDriveType(&drive[0]) != windows.DRIVE_FIXED {
		t.Skipf("TEMP is not on a local fixed drive: %q", base)
	}
	name, err := windows.UTF16PtrFromString(base)
	if err != nil {
		t.Fatal(err)
	}
	var volume [4]uint16
	if err := windows.GetVolumePathName(name, &volume[0], uint32(len(volume))); err != nil || volume != drive {
		t.Skipf("TEMP does not have an ordinary drive-root mount: %q (%v)", base, err)
	}
	identity, err := fileutil.ResolveExistingPath(base)
	if err != nil || identity != base {
		t.Skipf("TEMP does not retain its DOS handle identity: %q -> %q (%v)", base, identity, err)
	}
	home, root := filepath.Join(base, "home"), filepath.Join(base, "workspace")
	for _, dir := range []string{home, root} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(home, "config.toml"):   "[memory]\nrecall_limit = 17\n",
		filepath.Join(root, "reasonix.toml"): "[agent]\ncompact_ratio = 0.61\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("REASONIX_CREDENTIALS_STORE", "file")
	backend := evalSymlinks
	t.Cleanup(func() { evalSymlinks = backend })
	measure := func(resolve func(string) (string, error)) float64 {
		evalSymlinks = resolve
		var cfg *Config
		var loadErr error
		allocs := testing.AllocsPerRun(3, func() {
			var err error
			cfg, err = RootsForHome(home).LoadForRootReadOnly(root)
			if err != nil {
				loadErr = err
			}
		})
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if cfg.Memory.RecallLimit != 17 || cfg.Agent.CompactRatio != 0.61 || len(cfg.LoadWarnings()) != 0 {
			t.Fatalf("loaded recall/ratio = %d/%v, want 17/0.61; warnings=%v", cfg.Memory.RecallLimit, cfg.Agent.CompactRatio, cfg.LoadWarnings())
		}
		return allocs
	}
	sdk := measure(filepath.EvalSymlinks)
	actual := measure(backend)
	metric, err := json.Marshal(map[string]any{
		"code":               "config.path_allocations",
		"sdk_allocs":         sdk,
		"default_allocs":     actual,
		"runs":               3,
		"settings_validated": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CONFIG_ALLOCATION_EFFECT %s", metric)
	if actual >= sdk {
		t.Fatalf("public config load allocations = %v, SDK = %v; want fewer", actual, sdk)
	}
}
