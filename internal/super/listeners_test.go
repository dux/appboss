package super

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClearPortRangeTerminatesOnlyListenersInRange(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof is not installed")
	}
	inside, insidePort := startListenerHelper(t)
	outside, _ := startListenerHelper(t)
	pids, err := ClearPortRange([2]int{insidePort, insidePort}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 1 || pids[0] != inside.Process.Pid {
		t.Fatalf("cleared pids = %v, want [%d]", pids, inside.Process.Pid)
	}
	if err := inside.Wait(); err == nil {
		t.Fatal("listener exited without a signal")
	}
	if err := syscall.Kill(outside.Process.Pid, 0); err != nil {
		t.Fatalf("listener outside range was killed: %v", err)
	}
}

// doctor reports the address an app bound, not just that something is there, so the parse of
// lsof's field output has to survive a real process.
func TestListenersInRangeReportsAddress(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof is not installed")
	}
	helper, port := startListenerHelper(t)
	listeners, err := ListenersInRange([2]int{port, port})
	if err != nil {
		t.Fatal(err)
	}
	if len(listeners) != 1 {
		t.Fatalf("listeners = %+v, want one", listeners)
	}
	listener := listeners[0]
	if listener.PID != helper.Process.Pid || listener.Command == "" {
		t.Fatalf("unexpected listener: %+v", listener)
	}
	if !strings.HasPrefix(listener.Address, "127.0.0.1:") || !listener.Loopback() {
		t.Fatalf("helper binds loopback, got %+v", listener)
	}
}

func TestListenerLoopback(t *testing.T) {
	for address, want := range map[string]bool{
		"127.0.0.1:3101": true,
		"[::1]:3101":     true,
		"*:3101":         false,
		"[::]:3101":      false,
		"0.0.0.0:3101":   false,
		"10.0.0.5:3101":  false,
		"":               false,
	} {
		if got := (Listener{Address: address}).Loopback(); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", address, got, want)
		}
	}
}

func TestListenerHelperProcess(t *testing.T) {
	value := os.Getenv("BOSS_TEST_LISTENER_PORT")
	if value == "" {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+value)
	if err != nil {
		os.Exit(2)
	}
	fmt.Println("ready")
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			os.Exit(0)
		}
		_ = connection.Close()
	}
}

func startListenerHelper(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return startListenerHelperOnPort(t, port), port
}

func startListenerHelperOnPort(t *testing.T, port int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestListenerHelperProcess")
	cmd.Env = append(os.Environ(), "BOSS_TEST_LISTENER_PORT="+strconv.Itoa(port))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("listener helper did not start: %q", scanner.Text())
	}
	return cmd
}
