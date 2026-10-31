package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"testing"
	"time"

	"reasonix/internal/base/fileutil"
)

type configMechanismCached struct {
	path string
	err  error
}

type configMechanismCategory struct {
	Requests       int   `json:"requests"`
	ActualCalls    int   `json:"actual_calls"`
	FilepathCalls  int   `json:"filepath_calls"`
	HandleCalls    int   `json:"handle_calls"`
	Fallbacks      int   `json:"fallbacks"`
	Hits           int   `json:"hits"`
	BackendWallNS  int64 `json:"backend_wall_ns"`
	CallbackWallNS int64 `json:"callback_wall_ns"`
}

// Backend memoization is unsafe for changing filesystems. This experiment uses
// unchanged, admitted fixtures only; it is not a production fix or security proof.
func loadConfigMechanism(fixture configLoadFixture, runtime bool, observe bool) (*Config, map[string]*configMechanismCategory, time.Duration, error) {
	variant := os.Getenv("REASONIX_CONFIG_MECHANISM_VARIANT")
	memo, handle := variant == "backend-memo", variant == "handle-backend"
	var categories map[string]*configMechanismCategory
	if observe {
		categories = map[string]*configMechanismCategory{}
	}
	previous := evalSymlinks
	if memo || handle || observe {
		cache := map[string]configMechanismCached{}
		evalSymlinks = func(path string) (string, error) {
			var started time.Time
			var category *configMechanismCategory
			if observe {
				started = time.Now()
				name := "root_other"
				if path == fixture.userPath {
					name = "user_file"
				} else if path == fixture.projectPath {
					name = "project_file"
				}
				category = categories[name]
				if category == nil {
					category = &configMechanismCategory{}
					categories[name] = category
				}
				category.Requests++
			}
			if cached, ok := cache[path]; memo && ok {
				if observe {
					category.Hits++
					category.CallbackWallNS += time.Since(started).Nanoseconds()
				}
				return cached.path, cached.err
			}
			var backendStarted time.Time
			if observe {
				backendStarted = time.Now()
				category.ActualCalls++
			}
			var resolved string
			var err error
			if handle {
				resolved, err = fileutil.ResolveExistingPath(path)
				if observe {
					category.HandleCalls++
				}
				if err != nil {
					if observe {
						category.Fallbacks++
						category.FilepathCalls++
					}
					resolved, err = previous(path)
				}
			} else {
				resolved, err = previous(path)
				if observe {
					category.FilepathCalls++
				}
			}
			if observe {
				category.BackendWallNS += time.Since(backendStarted).Nanoseconds()
				category.CallbackWallNS += time.Since(started).Nanoseconds()
			}
			if memo {
				cache[path] = configMechanismCached{resolved, err}
			}
			return resolved, err
		}
	}
	defer func() { evalSymlinks = previous }()
	if !observe {
		cfg, err := fixture.load(runtime)
		return cfg, nil, 0, err
	}
	started := time.Now()
	cfg, err := fixture.load(runtime)
	return cfg, categories, time.Since(started), err
}

func BenchmarkConfigMechanismWarm(b *testing.B) {
	benchmarkConfigMechanism(b, true)
}

func BenchmarkConfigMechanismFirst(b *testing.B) {
	if os.Getenv("REASONIX_CONFIG_BENCH_ROOT") == "" {
		b.Fatal("first-load experiment requires externally prepared admitted fixtures")
	}
	benchmarkConfigMechanism(b, false)
}

func benchmarkConfigMechanism(b *testing.B, warm bool) {
	for _, profile := range configLoadProfiles {
		b.Run(profile.name, func(b *testing.B) {
			for _, mode := range []string{"readonly", "runtime"} {
				b.Run(mode, func(b *testing.B) {
					fixture := newConfigLoadFixture(b, profile)
					if warm {
						cfg, _, _, err := loadConfigMechanism(fixture, mode == "runtime", false)
						if err != nil {
							b.Fatal(err)
						}
						checkConfigLoadFixture(b, fixture, cfg)
					}
					var profileFile *os.File
					if path := os.Getenv("REASONIX_CONFIG_CPU_PROFILE"); path != "" {
						var err error
						profileFile, err = os.Create(path)
						if err != nil {
							b.Fatal(err)
						}
						if err := pprof.StartCPUProfile(profileFile); err != nil {
							_ = profileFile.Close()
							b.Fatal(err)
						}
					}
					b.ReportAllocs()
					var cfg *Config
					for b.Loop() {
						var err error
						cfg, _, _, err = loadConfigMechanism(fixture, mode == "runtime", false)
						if err != nil {
							b.Fatal(err)
						}
					}
					if profileFile != nil {
						pprof.StopCPUProfile()
						if err := profileFile.Close(); err != nil {
							b.Fatal(err)
						}
					}
					checkConfigLoadFixture(b, fixture, cfg)
				})
			}
		})
	}
}

func TestConfigMechanismAttribution(t *testing.T) {
	for _, profile := range configLoadProfiles {
		for _, mode := range []string{"readonly", "runtime"} {
			t.Run(profile.name+"/"+mode, func(t *testing.T) {
				fixture := newConfigLoadFixture(t, profile)
				cfg, categories, elapsed, err := loadConfigMechanism(fixture, mode == "runtime", true)
				if err != nil {
					t.Fatal(err)
				}
				checkConfigLoadFixture(t, fixture, cfg)
				data, err := json.Marshal(map[string]any{
					"profile": profile.name, "mode": mode,
					"variant":                         os.Getenv("REASONIX_CONFIG_MECHANISM_VARIANT"),
					"load_wall_ns_observer_inclusive": elapsed.Nanoseconds(), "categories": categories,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("CONFIG_MECHANISM_ATTRIBUTION %s", data)
			})
		}
	}
}

func BenchmarkConfigPathOperation(b *testing.B) {
	for _, profile := range configLoadProfiles {
		b.Run(profile.name, func(b *testing.B) {
			fixture := newConfigLoadFixture(b, profile)
			for _, source := range []struct{ name, path string }{{"user", fixture.userPath}, {"project", fixture.projectPath}} {
				b.Run(source.name, func(b *testing.B) {
					root, err := os.OpenRoot(filepath.Dir(source.path))
					if err != nil {
						b.Fatal(err)
					}
					defer root.Close()
					base := filepath.Base(source.path)
					operations := []struct {
						name string
						run  func() error
					}{
						{"EvalSymlinks", func() error { _, err := filepath.EvalSymlinks(source.path); return err }},
						{"HandleResolve", func() error { _, err := fileutil.ResolveExistingPath(source.path); return err }},
						{"AllowMissing", func() error { _, err := evalSymlinksAllowMissing(source.path); return err }},
						{"Lstat", func() error { _, err := os.Lstat(source.path); return err }},
						{"Stat", func() error { _, err := os.Stat(source.path); return err }},
						{"ReadFile", func() error { _, err := os.ReadFile(source.path); return err }},
						{"OpenRootClose", func() error {
							opened, err := os.OpenRoot(filepath.Dir(source.path))
							if err != nil {
								return err
							}
							return opened.Close()
						}},
						{"RootStat", func() error { _, err := root.Stat(base); return err }},
						{"RootReadFile", func() error { _, err := root.ReadFile(base); return err }},
					}
					for _, operation := range operations {
						b.Run(operation.name, func(b *testing.B) {
							for b.Loop() {
								if err := operation.run(); err != nil {
									b.Fatal(err)
								}
							}
						})
					}
				})
			}
		})
	}
}

func TestConfigMechanismControlDefinition(t *testing.T) {
	variant := os.Getenv("REASONIX_CONFIG_MECHANISM_VARIANT")
	if variant != "" && variant != "stock" && variant != "backend-memo" && variant != "handle-backend" {
		t.Fatal(fmt.Errorf("unknown static-fixture mechanism variant %q", variant))
	}
}
