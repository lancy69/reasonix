package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"reasonix/internal/base/testenv"
)

type configLoadProfile struct {
	name      string
	files     int
	depth     int
	providers int
	plugins   int
	mcp       int
}

var configLoadProfiles = []configLoadProfile{
	{"typical", 100, 4, 2, 2, 2},
	{"large-tree", 10000, 12, 2, 2, 2},
	{"large-config", 10000, 12, 64, 128, 96},
}

type configLoadFixture struct {
	root        string
	userPath    string
	projectPath string
	roots       Roots
	tomlBytes   int
	mcpBytes    int
}

func newConfigLoadFixture(t testing.TB, profile configLoadProfile) configLoadFixture {
	t.Helper()
	base := os.Getenv("REASONIX_CONFIG_BENCH_ROOT")
	if base == "" {
		base = testenv.TempDir(t)
	} else {
		base = filepath.Join(base, profile.name)
		if err := os.MkdirAll(base, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	home, root := filepath.Join(base, "home"), filepath.Join(base, "project")
	for range profile.depth {
		root = filepath.Join(root, "segment")
	}
	for _, dir := range []string{home, root} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("REASONIX_CREDENTIALS_STORE", "")
	fixture := configLoadFixture{root: root, roots: RootsForHome(home)}
	fixture.userPath, fixture.projectPath = fixture.roots.UserConfigPath(), filepath.Join(root, "reasonix.toml")
	user := "credentials_store = 'file'\ndefault_model = 'user-000/test'\n" + configLoadFixtureTOML("user", profile)
	project := "[memory]\nrecall_limit = 7\n" + configLoadFixtureTOML("project", profile)
	servers := map[string]any{}
	for i := range profile.mcp {
		servers[fmt.Sprintf("json-%03d", i)] = map[string]any{"command": "fixture-mcp", "args": []string{"serve"}}
	}
	mcp, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		fixture.userPath: []byte(user), fixture.projectPath: []byte(project), filepath.Join(root, ".mcp.json"): mcp,
		filepath.Join(home, ".env"): {},
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(configLoadTreeFile(root, profile.files-1)); os.IsNotExist(err) {
		for i := range profile.files {
			path := configLoadTreeFile(root, i)
			if i%100 == 0 {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, []byte("package fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	} else if err != nil {
		t.Fatal(err)
	}
	fixture.tomlBytes, fixture.mcpBytes = len(user)+len(project), len(mcp)
	return fixture
}

func configLoadFixtureTOML(scope string, profile configLoadProfile) string {
	var body strings.Builder
	for i := range profile.providers {
		fmt.Fprintf(&body, "[[providers]]\nname = '%s-%03d'\nkind = 'openai'\nbase_url = 'http://127.0.0.1:1/v1'\napi_key = 'dummy'\nmodels = ['test']\n", scope, i)
	}
	for i := range profile.plugins {
		fmt.Fprintf(&body, "[[plugins]]\nname = '%s-plugin-%03d'\ncommand = 'fixture-mcp'\nargs = ['serve']\n", scope, i)
	}
	return body.String()
}

func configLoadTreeFile(root string, i int) string {
	return filepath.Join(root, "src", fmt.Sprintf("package-%03d", i/100), fmt.Sprintf("source-%05d.go", i))
}

func (fixture configLoadFixture) load(runtime bool) (*Config, error) {
	if runtime {
		return fixture.roots.LoadForRoot(fixture.root)
	}
	return fixture.roots.LoadForRootReadOnly(fixture.root)
}

func checkConfigLoadFixture(t testing.TB, cfg *Config) {
	t.Helper()
	if cfg.DefaultModel != "user-000/test" || cfg.CredentialsStore != "file" || cfg.Memory.RecallLimit != 7 {
		t.Fatalf("load lost fixture settings: model=%q store=%q recall=%d", cfg.DefaultModel, cfg.CredentialsStore, cfg.Memory.RecallLimit)
	}
}

func BenchmarkConfigLoadWarm(b *testing.B) {
	benchmarkConfigLoad(b, true)
}

func BenchmarkConfigLoadFirst(b *testing.B) {
	benchmarkConfigLoad(b, false)
}

func benchmarkConfigLoad(b *testing.B, warm bool) {
	for _, profile := range configLoadProfiles {
		b.Run(profile.name, func(b *testing.B) {
			for _, mode := range []string{"readonly", "runtime"} {
				b.Run(mode, func(b *testing.B) {
					fixture := newConfigLoadFixture(b, profile)
					if warm {
						cfg, err := fixture.load(mode == "runtime")
						if err != nil {
							b.Fatal(err)
						}
						checkConfigLoadFixture(b, cfg)
					}
					b.ReportAllocs()
					var cfg *Config
					for b.Loop() {
						var err error
						cfg, err = fixture.load(mode == "runtime")
						if err != nil {
							b.Fatal(err)
						}
					}
					checkConfigLoadFixture(b, cfg)
					b.ReportMetric(float64(fixture.tomlBytes), "toml-B")
					b.ReportMetric(float64(fixture.mcpBytes), "mcp-B")
					b.ReportMetric(float64(profile.files), "tree-files")
					b.ReportMetric(float64(profile.depth), "path-depth")
				})
			}
		})
	}
}

func TestConfigLoadMeasurementCounts(t *testing.T) {
	for _, profile := range configLoadProfiles {
		for _, mode := range []string{"readonly", "runtime"} {
			t.Run(profile.name+"/"+mode, func(t *testing.T) {
				fixture := newConfigLoadFixture(t, profile)
				previous := evalSymlinks
				var mu sync.Mutex
				counts := map[string]int{}
				evalSymlinks = func(path string) (string, error) {
					mu.Lock()
					counts[path]++
					mu.Unlock()
					return previous(path)
				}
				defer func() { evalSymlinks = previous }()
				cfg, err := fixture.load(mode == "runtime")
				if err != nil {
					t.Fatal(err)
				}
				checkConfigLoadFixture(t, cfg)
				mu.Lock()
				defer mu.Unlock()
				total := 0
				for _, count := range counts {
					total += count
				}
				measurement, err := json.Marshal(map[string]any{
					"profile": profile.name, "mode": mode, "total": total,
					"user": counts[fixture.userPath], "project": counts[fixture.projectPath],
					"other":      total - counts[fixture.userPath] - counts[fixture.projectPath],
					"tree_files": profile.files, "path_depth": profile.depth,
					"providers_per_source": profile.providers, "plugins_per_source": profile.plugins,
					"toml_bytes": fixture.tomlBytes, "mcp_bytes": fixture.mcpBytes,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("CONFIG_LOAD_COUNTS %s", measurement)
			})
		}
	}
}
