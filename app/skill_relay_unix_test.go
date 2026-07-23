//go:build !windows

package app

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestRelayTimeoutHelperProcess(t *testing.T) {
	if os.Getenv("PIN_RELAY_TIMEOUT_HELPER") != "1" {
		return
	}

	child := exec.Command("sleep", "5")
	child.Stdout = os.Stdout
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
}

func TestRelayRunTimeoutBoundsInheritedOutputPipe(t *testing.T) {
	t.Setenv("PIN_RELAY_TIMEOUT_HELPER", "1")

	relay := relayAdapter{executable: os.Args[0]}
	start := time.Now()
	_, err := relay.run(250*time.Millisecond, "-test.run=^TestRelayTimeoutHelperProcess$")
	elapsed := time.Since(start)

	if err == nil || err.Error() != "relay command timed out" {
		t.Fatalf("relay.run error = %v, want timeout", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("relay.run returned after %s; inherited output pipe was not bounded", elapsed)
	}
}
