//go:build !windows

package attach

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"reasonix/internal/platform/remote/bootstrap"
	"reasonix/internal/platform/wsl"
	"reasonix/internal/state/store"
)

func TestWSLReleaseStopsDetachedKernels(t *testing.T) {
	for _, closePool := range []bool{false, true} {
		name := "release"
		if closePool {
			name = "pool close"
		}
		t.Run(name, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			run := func(ctx context.Context, stdin io.Reader, args ...string) (wsl.Result, error) {
				cmd := exec.CommandContext(ctx, args[3], args[4:]...)
				cmd.Env = append(os.Environ(), "HOME="+home)
				cmd.Stdin = stdin
				var out, errb bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &errb
				err := cmd.Run()
				res := wsl.Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					res.ExitCode, err = ee.ExitCode(), nil
				}
				return res, err
			}
			m := &wslMachine{t: t}
			raw, err := newWSLLink(t.Context(), wslEntry(), m.list, run)
			if err != nil {
				t.Fatal(err)
			}
			client := raw.(*wslLink)
			if err := client.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			p := NewPool(t.Context(), Options{})
			t.Cleanup(p.Close)
			l, _ := p.holdLink("dev")
			l.client = client
			close(l.ready)
			var exited []chan error
			for _, ws := range []string{"/one", "/two"} {
				dir := filepath.Join(home, ".reasonix", store.RemoteDirName)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				slug := store.RemoteWorkspaceSlug(ws)
				token := filepath.Join(dir, store.RemoteServeTokenName(slug))
				port := filepath.Join(dir, store.RemoteServePortName(slug))
				cmd := exec.Command("/bin/sh", "-c", "while :; do sleep 1; done", "reasonix", "serve", token, port)
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				t.Cleanup(func() { _ = cmd.Process.Kill() })
				exited = append(exited, done)
				data, err := bootstrap.MarshalState(bootstrap.ServeState{PID: cmd.Process.Pid, Workspace: ws})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, store.RemoteServeStateName(slug)), data, 0600); err != nil {
					t.Fatal(err)
				}
				client.retainKernel(ws)
			}
			s, _ := p.holdSpace(l, "/two")
			s.refs++
			l.refs++
			p.dropSpace(l, s, nil)
			for _, done := range exited {
				select {
				case err := <-done:
					t.Fatalf("shared link stopped a kernel early: %v", err)
				default:
				}
			}
			if closePool {
				p.Close()
			} else {
				p.dropSpace(l, s, nil)
			}
			for _, done := range exited {
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("kernel survived link release")
				}
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
