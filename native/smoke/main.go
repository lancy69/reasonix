package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"reasonix/internal/assembly/boot"
	"reasonix/internal/base/fileutil"
	"reasonix/internal/contract/config"
	"reasonix/internal/contract/event"
	_ "reasonix/internal/model/openai"
)

func emit(stage string, fields map[string]any) {
	fields["stage"] = stage
	fields["os"] = runtime.GOOS
	if err := json.NewEncoder(os.Stdout).Encode(fields); err != nil {
		panic(err)
	}
}

func userBody(recall int, model string) []byte {
	return fmt.Appendf(nil, `default_model = "fixture/%s"
[memory]
recall_limit = %d
[[providers]]
name = "fixture"
kind = "openai"
base_url = "http://127.0.0.1:1/v1"
model = "%s"
api_key_env = "REASONIX_12533_SMOKE_KEY"
`, model, recall, model)
}

func inspect(stage, home, root string, recall int, ratio float64, model string) error {
	wantRef := "fixture/" + model
	roots := config.RootsForHome(home)
	for _, readOnly := range []bool{true, false} {
		var cfg *config.Config
		var err error
		if readOnly {
			cfg, err = roots.LoadForRootReadOnly(root)
		} else {
			cfg, err = roots.LoadForRoot(root)
		}
		if err != nil {
			return fmt.Errorf("%s load readOnly=%t: %w", stage, readOnly, err)
		}
		entry, ok := cfg.ResolveModel(cfg.DefaultModel)
		if !ok || entry.Model != model || cfg.DefaultModel != wantRef || cfg.Memory.RecallLimit != recall || cfg.Agent.CompactRatio != ratio || len(cfg.LoadWarnings()) != 0 {
			return fmt.Errorf("%s unexpected load: default=%q recall=%d ratio=%v resolved=%t warnings=%v", stage, cfg.DefaultModel, cfg.Memory.RecallLimit, cfg.Agent.CompactRatio, ok, cfg.LoadWarnings())
		}
		emit(stage+"-load", map[string]any{"read_only": readOnly, "recall": recall, "compact_ratio": ratio, "selected_model": cfg.DefaultModel, "provider_model": entry.Model, "warnings": cfg.LoadWarnings()})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctrl, err := boot.Build(ctx, boot.Options{Home: home, WorkspaceRoot: root, RequireKey: true, Sink: event.Discard, Stderr: io.Discard})
	if err != nil {
		return fmt.Errorf("%s Build: %w", stage, err)
	}
	defer ctrl.Close()
	if ctrl.ModelRef() != wantRef || ctrl.WorkspaceRoot() != root {
		return fmt.Errorf("%s Build selected model=%q workspace=%q, want %q/%q", stage, ctrl.ModelRef(), ctrl.WorkspaceRoot(), wantRef, root)
	}
	emit(stage+"-Build", map[string]any{"selected_model": ctrl.ModelRef(), "workspace": ctrl.WorkspaceRoot(), "model_request_executed": false})
	return nil
}

func run() error {
	base, err := os.MkdirTemp("", "reasonix-12533-narrow-smoke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(base)
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	home, root := filepath.Join(base, "home"), filepath.Join(base, "workspace")
	for _, dir := range []string{home, root} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
	}
	for key, value := range map[string]string{
		"REASONIX_HOME":            home,
		"REASONIX_STATE_HOME":      filepath.Join(base, "state"),
		"REASONIX_CACHE_HOME":      filepath.Join(base, "cache"),
		"REASONIX_12533_SMOKE_KEY": "dummy-smoke-key-not-a-credential",
	} {
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	userPath, projectPath := filepath.Join(home, "config.toml"), filepath.Join(root, "reasonix.toml")
	if err := fileutil.AtomicWriteFile(userPath, userBody(17, "first"), 0o600); err != nil {
		return err
	}
	if err := fileutil.AtomicWriteFile(projectPath, []byte("[agent]\ncompact_ratio = 0.61\n"), 0o600); err != nil {
		return err
	}
	for _, mode := range []string{"default", "file"} {
		if mode == "default" {
			err = os.Unsetenv("REASONIX_CREDENTIALS_STORE")
		} else {
			err = os.Setenv("REASONIX_CREDENTIALS_STORE", "file")
		}
		if err != nil {
			return err
		}
		if err := inspect(mode, home, root, 17, 0.61, "first"); err != nil {
			return err
		}
	}
	if err := fileutil.AtomicWriteFile(userPath, userBody(29, "second"), 0o600); err != nil {
		return err
	}
	if _, err := config.SaveMinimalProjectCompactRatio(projectPath, 0.72); err != nil {
		return err
	}
	if err := inspect("atomic-replacement", home, root, 29, 0.72, "second"); err != nil {
		return err
	}
	for _, item := range []struct {
		model  string
		recall int
	}{{"third", 47}, {"fourth", 53}} {
		target := filepath.Join(base, item.model+".toml")
		if err := fileutil.AtomicWriteFile(target, userBody(item.recall, item.model), 0o600); err != nil {
			return err
		}
		if err := os.Remove(userPath); err != nil {
			return err
		}
		if err := os.Symlink(target, userPath); err != nil {
			emit("links-unavailable", map[string]any{"error": err.Error(), "user_links_and_project_escape_exercised": false})
			return nil
		}
		if err := inspect("user-link-"+item.model, home, root, item.recall, 0.72, item.model); err != nil {
			return err
		}
	}
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		return err
	}
	outsideConfig := filepath.Join(outside, "reasonix.toml")
	if err := fileutil.AtomicWriteFile(outsideConfig, []byte("[memory]\nrecall_limit = 71\n"), 0o600); err != nil {
		return err
	}
	if err := os.Remove(projectPath); err != nil {
		return err
	}
	if err := os.Symlink(outsideConfig, projectPath); err != nil {
		return err
	}
	if _, err := config.RootsForHome(home).LoadForRootReadOnly(root); err == nil {
		return fmt.Errorf("project config escape was accepted")
	}
	emit("project-file-escape", map[string]any{"refused": true})
	if err := os.Remove(projectPath); err != nil {
		return err
	}
	if err := os.Symlink(outside, filepath.Join(root, ".reasonix")); err != nil {
		return err
	}
	if _, err := config.RootsForHome(home).LoadForRootReadOnly(root); err == nil {
		return fmt.Errorf("nested project config escape was accepted")
	}
	emit("nested-project-escape", map[string]any{"refused": true})
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
