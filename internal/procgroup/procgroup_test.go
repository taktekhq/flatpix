//go:build unix

package procgroup

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the helper child: the test binary re-executes itself.
func TestMain(m *testing.M) {
	switch os.Getenv("PROCGROUP_HELPER") {
	case "spawn-grandchild":
		// Start a grandchild that would outlive us, report its pid, then hang.
		gc := exec.Command("sleep", "300")
		if err := gc.Start(); err != nil {
			os.Exit(3)
		}
		os.WriteFile(os.Getenv("PIDFILE"), []byte(strconv.Itoa(gc.Process.Pid)), 0o600)
		time.Sleep(300 * time.Second)
		os.Exit(0)
	case "env":
		os.Stdout.WriteString(strings.Join(os.Environ(), "\n"))
		os.Exit(0)
	case "fail":
		os.Stderr.WriteString("boom")
		os.Exit(7)
	}
	os.Exit(m.Run())
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestKillsTheWholeProcessGroupOnCancel(t *testing.T) {
	pidfile := t.TempDir() + "/gc.pid"
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- Run(ctx, Cmd{
			Path: os.Args[0],
			Env:  append(os.Environ(), "PROCGROUP_HELPER=spawn-grandchild", "PIDFILE="+pidfile),
		})
	}()
	var gcPid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidfile); err == nil && len(b) > 0 {
			gcPid, _ = strconv.Atoi(string(b))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gcPid == 0 {
		t.Fatal("grandchild never started")
	}
	if !alive(gcPid) {
		t.Fatal("grandchild not running")
	}
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("want an error after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	deadline = time.Now().Add(3 * time.Second)
	for alive(gcPid) {
		// A killed child is a zombie until its parent (init) reaps it.
		if time.Now().After(deadline) {
			if isZombie(gcPid) {
				return
			}
			t.Fatalf("grandchild %d survived the group kill", gcPid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isZombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(b, ')')
	return i > 0 && len(b) > i+2 && b[i+2] == 'Z'
}

func TestTimeoutKillsToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Run(ctx, Cmd{Path: os.Args[0], Env: append(os.Environ(), "PROCGROUP_HELPER=spawn-grandchild", "PIDFILE="+t.TempDir()+"/p")})
	if err == nil {
		t.Fatal("want an error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestChildEnvPinsOneThread(t *testing.T) {
	env := ChildEnv([]string{"TMPDIR=/s", "MAGICK_TMPDIR=/s"}, []string{"PATH=/bin", "SECRET=x"})
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	for k, want := range map[string]string{
		"MAGICK_THREAD_LIMIT": "1", "OMP_NUM_THREADS": "1", "VIPS_CONCURRENCY": "1",
		"TMPDIR": "/s", "MAGICK_TMPDIR": "/s", "FLATPIX_CHILD": "render", "VIPS_BLOCK_UNTRUSTED": "1",
	} {
		if m[k] != want {
			t.Errorf("%s = %q, want %q", k, m[k], want)
		}
	}
	if _, ok := m["SECRET"]; ok {
		t.Error("the parent's secrets must not reach the child")
	}
	if m["PATH"] != "/bin" {
		t.Error("PATH should pass through")
	}
}

func TestExitCodeAndStderr(t *testing.T) {
	var stderr bytes.Buffer
	err := Run(context.Background(), Cmd{Path: os.Args[0], Env: append(os.Environ(), "PROCGROUP_HELPER=fail"), Stderr: &stderr})
	var ee *exec.ExitError
	if err == nil || !asExit(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("err = %v", err)
	}
	if stderr.String() != "boom" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func asExit(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}
