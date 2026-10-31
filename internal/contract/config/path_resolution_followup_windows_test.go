package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/base/fileutil"
	"reasonix/internal/base/testenv"
)

func noAliasError(err error) any {
	if err == nil {
		return nil
	}
	row := map[string]any{"type": fmt.Sprintf("%T", err), "text": err.Error(), "not_exist": os.IsNotExist(err), "permission": os.IsPermission(err)}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		row["op"], row["path"] = pathErr.Op, pathErr.Path
	}
	return row
}

func noAliasConsume(path string) map[string]any {
	info, statErr := os.Stat(path)
	data, readErr := os.ReadFile(path)
	row := map[string]any{"path": path, "stat_error": noAliasError(statErr), "read_error": noAliasError(readErr), "contents": string(data)}
	if statErr == nil {
		row["size"] = info.Size()
	}
	file, openErr := os.Open(path)
	row["open_error"] = noAliasError(openErr)
	if openErr == nil {
		openedData, openedErr := io.ReadAll(io.LimitReader(file, 4096))
		row["open_contents"], row["open_read_error"], row["close_error"] = string(openedData), noAliasError(openedErr), noAliasError(file.Close())
	}
	return row
}

func noAliasEmit(t *testing.T, row map[string]any) {
	t.Helper()
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CONFIG_HANDLE_FOLLOWUP %s", data)
}

func TestConfigHandleBackendFollowup(t *testing.T) {
	root, outside := testenv.TempDir(t), testenv.TempDir(t)
	ordinaryAlias := filepath.Join(outside, "literal-home-space")
	literalHome := `\\?\` + filepath.Join(outside, "literal-home-space ")
	if err := os.MkdirAll(literalHome, 0o700); err != nil {
		t.Fatal(err)
	}
	literalConfig := filepath.Join(literalHome, "config.toml")
	if err := os.WriteFile(literalConfig, []byte("[memory]\nrecall_limit=1008\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(literalHome, ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ordinaryAlias, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ordinaryAlias, "config.toml"), []byte("[memory]\nrecall_limit=937\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noAliasEmit(t, map[string]any{"case": "owned-alias-before-removal", "alias_config": noAliasConsume(filepath.Join(ordinaryAlias, "config.toml")), "literal_config": noAliasConsume(literalConfig)})
	if err := os.RemoveAll(ordinaryAlias); err != nil {
		t.Fatal(err)
	}
	literalData, literalErr := os.ReadFile(literalConfig)
	if literalErr != nil || string(literalData) != "[memory]\nrecall_limit=1008\n" {
		t.Fatalf("literal fixture changed after removing owned ordinary alias: %q %v", literalData, literalErr)
	}
	_, aliasErr := os.Stat(ordinaryAlias)
	noAliasEmit(t, map[string]any{"case": "no-normal-alias-fixture", "ordinary_alias": ordinaryAlias, "ordinary_alias_error": noAliasError(aliasErr), "ordinary_alias_missing": os.IsNotExist(aliasErr), "literal_config": noAliasConsume(literalConfig)})
	if !os.IsNotExist(aliasErr) {
		t.Fatal("no-alias precondition failed")
	}
	for _, handle := range []bool{false, true} {
		func() {
			homeArgument := literalHome + `\`
			t.Setenv("REASONIX_HOME", homeArgument)
			t.Setenv("REASONIX_CREDENTIALS_STORE", "file")
			roots := RootsForHome(homeArgument)
			previous := evalSymlinks
			var backendRows []map[string]any
			evalSymlinks = func(path string) (string, error) {
				var resolved string
				var err error
				if handle {
					resolved, err = fileutil.ResolveExistingPath(path)
					if err != nil {
						resolved, err = filepath.EvalSymlinks(path)
					}
				} else {
					resolved, err = filepath.EvalSymlinks(path)
				}
				if path == roots.UserConfigPath() {
					row := map[string]any{"input": path, "resolved": resolved, "error": noAliasError(err)}
					if err == nil {
						row["consumption"] = noAliasConsume(resolved)
					}
					backendRows = append(backendRows, row)
				}
				return resolved, err
			}
			defer func() { evalSymlinks = previous }()
			cfg, err := roots.LoadForRootReadOnly(root)
			row := map[string]any{"case": "public-no-normal-alias-home-space", "handle_backend": handle, "home_argument": homeArgument, "user_path": roots.UserConfigPath(), "error": noAliasError(err), "config_returned": cfg != nil, "expected_recall": 1008, "backend_user_config_rows": backendRows}
			if cfg != nil {
				row["recall"], row["matches_expected"], row["warnings"], row["ignored"] = cfg.Memory.RecallLimit, cfg.Memory.RecallLimit == 1008, cfg.LoadWarnings(), cfg.IgnoredProjectSettings()
			}
			noAliasEmit(t, row)
		}()
	}
	t.Log("CONFIG_HANDLE_FOLLOWUP_SCOPE constructed explicit USER-home filesystem state with no ordinary trimmed alias; no observed production incident; no timing/profile measurement")
}
