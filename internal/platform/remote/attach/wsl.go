package attach

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"reasonix/internal/contract/config"
	"reasonix/internal/platform/remote"
	"reasonix/internal/platform/remote/bootstrap"
	"reasonix/internal/platform/remote/forward"
	"reasonix/internal/platform/remote/wslconn"
	"reasonix/internal/platform/wsl"
)

// ErrNoRemoteForward: a WSL distribution has no channel to publish a port of
// this machine into, so the one thing that would ask for it must not be sent.
var ErrNoRemoteForward = errors.New("attach: a WSL machine cannot be given a port of this one")

// wslLink is a WSL distribution as a Link. The distribution's loopback is
// reachable from here, so a "forward" is the address itself: nothing listens
// on this side and nothing has to be torn down.
type wslLink struct {
	*wslconn.Conn
	host string

	anchorStop context.CancelFunc
	anchorDone chan struct{}
	anchorIn   io.Writer // guarded by mu; nil while no session is up
	anchorUp   chan struct{}
	upOnce     sync.Once
	kernels    map[string]struct{}
	closeOnce  sync.Once
	closeErr   error

	mu   sync.Mutex
	subs map[int]func(remote.StatusEvent)
	next int
	last remote.StatusEvent
	have bool
}

func newWSLLink(ctx context.Context, entry config.RemoteHostEntry, list wsl.Runner, run wsl.Command) (Link, error) {
	conn, err := wslconn.New(ctx, list, run, entry.Distro)
	if err != nil {
		return nil, err
	}
	return &wslLink{Conn: conn, host: entry.Name, subs: map[int]func(remote.StatusEvent){}, anchorUp: make(chan struct{})}, nil
}

func (l *wslLink) publish(ev remote.StatusEvent) {
	ev.Host, ev.At = l.host, time.Now()
	l.mu.Lock()
	l.last, l.have = ev, true
	fns := make([]func(remote.StatusEvent), 0, len(l.subs))
	for _, fn := range l.subs {
		fns = append(fns, fn)
	}
	l.mu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

// requiredTools prints the first program a kernel launch depends on that the
// distribution lacks and stops with exitMissingTool. Minimal images omit ps,
// which is how the launched process is identified as the kernel.
const (
	requiredTools   = `for t in ps nohup; do command -v "$t" >/dev/null 2>&1 || { echo "$t"; exit 46; }; done`
	exitMissingTool = 46
)

// Workspace spells a folder the way the distribution sees it: a Windows path
// is translated by the distribution itself, anything else is already its own.
func (l *wslLink) Workspace(ctx context.Context, p string) (string, error) {
	if !wsl.IsWindowsPath(p) {
		return p, nil
	}
	return l.ToLinux(ctx, p)
}

// Start wakes the distribution: the first command is what boots it, and a
// distribution that cannot answer one is reported as the connect failing.
func (l *wslLink) Start(ctx context.Context) error {
	l.publish(remote.StatusEvent{Status: remote.StatusConnecting})
	res, err := l.Exec(ctx, requiredTools)
	switch {
	case err != nil:
	case res.ExitCode == exitMissingTool:
		err = fmt.Errorf("%w: %q has no %s, which starting a kernel there needs", wsl.ErrUnsupported, l.Distro(), strings.TrimSpace(string(res.Stdout)))
	case res.ExitCode != 0:
		err = fmt.Errorf("%w: %q answered with status %d", wsl.ErrUnsupported, l.Distro(), res.ExitCode)
	}
	if err != nil {
		l.publish(remote.StatusEvent{Status: remote.StatusStopped, Err: err})
		return err
	}
	l.publish(remote.StatusEvent{Status: remote.StatusConnected})
	anchorCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	l.anchorStop, l.anchorDone = stop, make(chan struct{})
	go l.holdAnchor(anchorCtx)
	select {
	case <-l.anchorUp:
	case <-ctx.Done():
		stop()
		<-l.anchorDone
		return ctx.Err()
	}
	return nil
}

// anchorRetry is how long a lost anchor waits before the distribution is asked
// for another.
var anchorRetry = 2 * time.Second

// holdAnchor feeds launch commands through a persistent shell. Whether its
// detached children survive a lost session depends on WSL; Close explicitly
// stops the kernels instead of relying on session teardown.
func (l *wslLink) holdAnchor(ctx context.Context) {
	defer close(l.anchorDone)
	for attempt := 1; ctx.Err() == nil; attempt++ {
		pr, pw := io.Pipe()
		l.mu.Lock()
		l.anchorIn = pw
		l.mu.Unlock()
		l.upOnce.Do(func() { close(l.anchorUp) })
		sessionDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = pw.Close()
			case <-sessionDone:
			}
		}()
		_, err := l.Run(ctx, pr, "/bin/sh")
		close(sessionDone)
		l.mu.Lock()
		l.anchorIn = nil
		l.mu.Unlock()
		_ = pw.Close()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("%w: the session holding %q open ended", wsl.ErrUnsupported, l.Distro())
		}
		l.publish(remote.StatusEvent{Status: remote.StatusReconnecting, Attempt: attempt, Err: err})
		select {
		case <-ctx.Done():
			return
		case <-time.After(anchorRetry):
		}
		l.publish(remote.StatusEvent{Status: remote.StatusConnected, Attempt: attempt})
	}
}

// ErrNoSession: the session that launches long-lived processes is not up, or
// ended before the command it was given finished.
var ErrNoSession = errors.New("attach: the WSL session that keeps a kernel alive is not open")

// ErrUnsafeScratch: the directory launch commands are staged in is not the
// current user's private directory, so what is written there could be read or
// replaced by someone else.
var ErrUnsafeScratch = errors.New("attach: the WSL launch directory is not private to this user")

// detachedPoll is how often a launched command is asked whether it finished.
const detachedPoll = 100 * time.Millisecond

const (
	exitUnsafeScratch = 47
	exitNotYet        = 44
	// scratch is a directory under the account's home that must be a real
	// directory it owns and nobody else can enter. A path under /tmp would be
	// accepted by mkdir -p whoever had made it.
	scratch = `d="$HOME/.reasonix/link"`
)

// ExecDetached runs cmd inside the held session, so what it starts is not
// ended when this call returns. The command is written to a file and only its
// name is typed into the session, which keeps the script's text out of the
// session's input. The wait ends when the command does or when the session it
// was given to does, whichever is first.
func (l *wslLink) ExecDetached(ctx context.Context, cmd string) (remote.ExecResult, error) {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	put, err := l.Script(ctx, strings.NewReader(cmd), scratch+`
mkdir -p -m 700 "$d" || exit 1
[ -d "$d" ] && [ ! -L "$d" ] && [ "$(stat -c %u "$d")" = "$(id -u)" ] && [ "$(stat -c %a "$d")" = 700 ] || exit 47
cat > "$d/$1.sh"`, id)
	if err != nil {
		return remote.ExecResult{}, err
	}
	switch put.ExitCode {
	case 0:
	case exitUnsafeScratch:
		return remote.ExecResult{}, ErrUnsafeScratch
	default:
		return remote.ExecResult{}, fmt.Errorf("attach: stage the launch command: exit %d: %s", put.ExitCode, strings.TrimSpace(string(put.Stderr)))
	}
	in := l.session()
	if in == nil {
		return remote.ExecResult{}, ErrNoSession
	}
	line := fmt.Sprintf(scratch+`; /bin/sh "$d/%[1]s.sh" </dev/null >"$d/%[1]s.out" 2>"$d/%[1]s.err"; echo $? >"$d/%[1]s.rc"`+"\n", id)
	wrote := make(chan error, 1)
	go func() { _, err := io.WriteString(in, line); wrote <- err }()
	select {
	case err := <-wrote:
		if err != nil {
			return remote.ExecResult{}, fmt.Errorf("%w: %w", ErrNoSession, err)
		}
	case <-ctx.Done():
		return remote.ExecResult{}, ctx.Err()
	}
	for {
		res, err := l.Script(ctx, nil, scratch+`; [ -s "$d/$1.rc" ] || exit 44`, id)
		if err != nil {
			return remote.ExecResult{}, err
		}
		if res.ExitCode != exitNotYet {
			break
		}
		if l.session() != in {
			return remote.ExecResult{}, ErrNoSession
		}
		select {
		case <-ctx.Done():
			return remote.ExecResult{}, ctx.Err()
		case <-time.After(detachedPoll):
		}
	}
	return l.collect(ctx, id)
}

// session is the input of the shell session currently held, nil when none is.
func (l *wslLink) session() io.Writer {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.anchorIn
}

// collect reads what a finished launch printed and how it ended, then removes
// its files. The status is read from its own file, so no exit code of the
// command can be mistaken for the poll's "not yet".
func (l *wslLink) collect(ctx context.Context, id string) (remote.ExecResult, error) {
	read := func(name string) ([]byte, error) {
		res, err := l.Script(ctx, nil, scratch+`; cat "$d/$1.`+name+`"`, id)
		return res.Stdout, err
	}
	out, err := read("out")
	if err != nil {
		return remote.ExecResult{}, err
	}
	errb, err := read("err")
	if err != nil {
		return remote.ExecResult{}, err
	}
	rc, err := read("rc")
	if err != nil {
		return remote.ExecResult{}, err
	}
	_, _ = l.Script(ctx, nil, scratch+`; rm -f "$d/$1.sh" "$d/$1.out" "$d/$1.err" "$d/$1.rc"`, id)
	code, err := strconv.Atoi(strings.TrimSpace(string(rc)))
	if err != nil {
		return remote.ExecResult{}, fmt.Errorf("attach: the launch left no readable status: %w", err)
	}
	return remote.ExecResult{Stdout: out, Stderr: errb, ExitCode: code}, nil
}

func (l *wslLink) retainKernel(workspace string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.kernels == nil {
		l.kernels = make(map[string]struct{})
	}
	l.kernels[workspace] = struct{}{}
}

func (l *wslLink) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		workspaces := make([]string, 0, len(l.kernels))
		for workspace := range l.kernels {
			workspaces = append(workspaces, workspace)
		}
		l.mu.Unlock()
		for _, workspace := range workspaces {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := bootstrap.Stop(ctx, l.Conn, workspace)
			cancel()
			if err != nil {
				l.closeErr = errors.Join(l.closeErr, fmt.Errorf("stop WSL kernel for %s: %w", workspace, err))
			}
		}
		if l.anchorStop != nil {
			l.anchorStop()
			<-l.anchorDone
		}
		l.publish(remote.StatusEvent{Status: remote.StatusStopped, Err: l.closeErr})
	})
	return l.closeErr
}

func (l *wslLink) Subscribe(fn func(remote.StatusEvent)) func() {
	l.mu.Lock()
	id := l.next
	l.next++
	l.subs[id] = fn
	last, have := l.last, l.have
	l.mu.Unlock()
	if have {
		fn(last)
	}
	return func() {
		l.mu.Lock()
		delete(l.subs, id)
		l.mu.Unlock()
	}
}

func (l *wslLink) Forwards() Forwards { return loopbackForwards{} }

// loopbackForwards reaches a port on the distribution's loopback directly.
type loopbackForwards struct{}

func (loopbackForwards) Add(spec forward.Spec) (string, error) {
	if spec.Direction != forward.Local {
		return "", ErrNoRemoteForward
	}
	return spec.TargetAddr, nil
}

func (loopbackForwards) List() []forward.Entry { return nil }

func (loopbackForwards) Remove(string) error { return nil }
