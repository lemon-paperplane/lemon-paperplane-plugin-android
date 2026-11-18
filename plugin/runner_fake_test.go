//go:build !windows

package android

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeADB 是一个假的 adb：resolve-activity / am start / pidof / logcat / get-state，按环境变量模拟应用活多久、是否崩溃。
const fakeADB = `#!/bin/sh
shift 2
S="$FAKE_DIR"
case "$1" in
  shell)
    shift
    case "$1" in
      cmd) echo "priority=0 preferredOrder=0 match=0x108000"; echo "com.a/.MainActivity" ;;
      am)
        if [ "$2" = "start" ]; then echo "Status: ok"; echo "Complete"; else echo "$@" >> "$S/forcestop"; fi ;;
      pidof)
        n=$(cat "$S/n" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$S/n"
        if [ $n -le "$FAKE_ALIVE" ]; then echo 4242; elif [ $n -le "$FAKE_RESTART" ]; then echo 4343; else exit 1; fi ;;
    esac ;;
  logcat)
    echo "11-18 10:00:00.000  4242  4242 I Test: hello"
    if [ -n "$FAKE_CRASH" ]; then echo "11-18 10:00:00.100  4242  4242 E AndroidRuntime: FATAL EXCEPTION: main"; fi
    exec sleep 30 ;;
  get-state) echo device ;;
esac
`

// syncBuf 是并发安全的输出缓冲，出现某个子串时通知。
type syncBuf struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want string
	hit  chan struct{}
	once sync.Once
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if b.want != "" && strings.Contains(b.buf.String(), b.want) {
		b.once.Do(func() { close(b.hit) })
	}
	return n, err
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newFakeRunner(t *testing.T, alive, restartUntil int, crash bool) (*runner, *syncBuf, string) {
	dir := t.TempDir()
	adb := filepath.Join(dir, "adb")
	if err := os.WriteFile(adb, []byte(fakeADB), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "FAKE_DIR="+dir, "FAKE_ALIVE="+strconv.Itoa(alive), "FAKE_RESTART="+strconv.Itoa(restartUntil))
	if crash {
		env = append(env, "FAKE_CRASH=1")
	}
	b := bundle(t, "en")
	out := &syncBuf{hit: make(chan struct{})}
	r := &runner{args: runnerArgs{ADB: adb, Serial: "emulator-5554", Package: "com.a", ForceStop: true}, l10n: b,
		adb: &adbClient{path: adb, env: env}, out: out, errOut: out}
	return r, out, dir
}

func TestRunnerAppExits(t *testing.T) {
	r, out, _ := newFakeRunner(t, 2, 2, false)
	code := r.main()
	s := out.String()
	if code != 0 || !strings.Contains(s, "App started: com.a (pid 4242)") || !strings.Contains(s, "I Test: hello") || !strings.Contains(s, "App exited: com.a") {
		t.Fatalf("code %d output:\n%s", code, s)
	}
	if !strings.Contains(s, "Launching com.a/.MainActivity") {
		t.Fatalf("activity not resolved:\n%s", s)
	}
}

func TestRunnerCrash(t *testing.T) {
	r, out, _ := newFakeRunner(t, 2, 2, true)
	if code := r.main(); code != 1 || !strings.Contains(out.String(), "App crashed: com.a") {
		t.Fatalf("code %d output:\n%s", code, out.String())
	}
}

func TestRunnerFollowsRestart(t *testing.T) {
	r, out, _ := newFakeRunner(t, 2, 4, false)
	if code := r.main(); code != 0 || !strings.Contains(out.String(), "App process restarted (pid 4343)") {
		t.Fatalf("code %d output:\n%s", code, out.String())
	}
}

func TestRunnerStopForceStops(t *testing.T) {
	r, out, dir := newFakeRunner(t, 9, 9, false)
	out.want = "I Test: hello"
	done := make(chan int, 1)
	go func() { done <- r.main() }()
	select {
	case <-out.hit:
	case <-time.After(10 * time.Second):
		t.Fatalf("never started:\n%s", out.String())
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not stop")
	}
	fs, _ := os.ReadFile(filepath.Join(dir, "forcestop"))
	if !strings.Contains(string(fs), "force-stop com.a") || !strings.Contains(out.String(), "Stopped app com.a") {
		t.Fatalf("force-stop not run: %q\n%s", fs, out.String())
	}
}
