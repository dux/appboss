package super

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
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
	return cmd, port
}
