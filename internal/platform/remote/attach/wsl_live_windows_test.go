//go:build windows

package attach

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"reasonix/internal/base/testenv"
	"reasonix/internal/contract/config"
	"reasonix/internal/platform/wsl"
)

// REASONIX_LIVE_WSL names an installed WSL2 distribution that already has a
// reasonix 2.x on its PATH (install "never"); the test brings up a kernel in it
// through the pool and calls it from Windows over loopback.
func TestLiveAttachBringsUpAKernelInADistribution(t *testing.T) {
	name := os.Getenv("REASONIX_LIVE_WSL")
	if name == "" {
		t.Skip("set REASONIX_LIVE_WSL to a WSL2 distribution with reasonix installed")
	}
	t.Setenv("REASONIX_HOME", testenv.TempDir(t))
	if err := config.EditUserConfigWithCredentials(func(c *config.Config) ([]config.CredentialChange, error) {
		return nil, c.UpsertRemoteHost(config.RemoteHostEntry{Name: "dev", Kind: "wsl", Distro: name, ServeInstall: "never"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := wsl.SystemCommand()(t.Context(), nil, wsl.ExecArgs(name, "/bin/mkdir", "-p", "/tmp/reasonix-live-workspace")...); err != nil {
		t.Fatal(err)
	}
	p := NewPool(t.Context(), Options{Install: "never"})
	defer p.Close()
	ep, err := p.Attach(t.Context(), "dev", "/tmp/reasonix-live-workspace", Call{Progress: func(step, detail string) { t.Logf("step %s %s", step, detail) }})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Logf("kernel at %s workspace %s", ep.Addr, ep.Workspace)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ep.Addr+"/", nil)
	req.Header.Set("Authorization", "Bearer "+ep.Token)
	var resp *http.Response
	for range 20 {
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the kernel did not answer over loopback: %v", err)
	}
	resp.Body.Close()
	t.Logf("GET / = %d", resp.StatusCode)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode >= 500 {
		t.Fatalf("the kernel refused the token it was started with: %d", resp.StatusCode)
	}

	// A second pane on the same workspace shares the one kernel.
	again, err := p.Attach(t.Context(), "dev", "/tmp/reasonix-live-workspace", Call{})
	if err != nil || again.Addr != ep.Addr {
		t.Fatalf("second attach = %+v, %v; want the same address %s", again, err, ep.Addr)
	}
	again.Release()

	// The same folder named the Windows way reaches the same kernel.
	unc, err := p.Attach(t.Context(), "dev", `\\wsl.localhost\`+name+`\tmp\reasonix-live-workspace`, Call{})
	if err != nil || unc.Addr != ep.Addr || unc.Workspace != ep.Workspace {
		t.Fatalf("UNC attach = %+v, %v; want the kernel at %s for %s", unc, err, ep.Addr, ep.Workspace)
	}
	unc.Release()
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatalf("releasing shared panes stopped the kernel: %v", err)
	} else {
		resp.Body.Close()
	}
	ep.Release()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		check, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ep.Addr+"/", nil)
		resp, err := http.DefaultClient.Do(check)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("kernel did not stop before the shutdown deadline")
			}
			break
		}
		resp.Body.Close()
		select {
		case <-ctx.Done():
			t.Fatal("kernel is still serving after final release")
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Log("kernel no longer answers over loopback after final release")
}
