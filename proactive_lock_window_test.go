package main

// Cross-process lock-window regression. The Java side starts this test binary,
// waits for the before-commit signal, writes the settings through the real
// native SettingsFile path (which takes the same lock), then releases it.
// Run only when OQ_LOCK_DIR is set; a normal `go test ./...` skips it.

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProactiveLockWindow(t *testing.T) {
	dir := os.Getenv("OQ_LOCK_DIR")
	if dir == "" {
		t.Skip("OQ_LOCK_DIR not set")
	}
	fixture := os.Getenv("OQ_LOCK_FIXTURE")
	if fixture == "" {
		t.Fatal("OQ_LOCK_FIXTURE not set")
	}
	s, err := OpenStore(filepath.Join(dir, "synthetic.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f, err := os.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		f.Close()
		t.Fatal("empty fixture")
	}
	if _, err := s.androidRequest(scanner.Bytes()); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if err := os.WriteFile(filepath.Join(dir, androidSettingsFile),
		[]byte(`{"proactive":true,"backgroundPrepare":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	signal := os.Getenv("OQ_LOCK_SIGNAL")
	release := os.Getenv("OQ_LOCK_RELEASE")
	result := os.Getenv("OQ_LOCK_RESULT")
	proactiveTickHook = func(stage string) error {
		if stage != "before-commit" {
			return nil
		}
		if err := os.WriteFile(signal, []byte("ready"), 0o600); err != nil {
			return err
		}
		for i := 0; i < 1000; i++ {
			if _, err := os.Stat(release); err == nil {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return errors.New("release timeout")
	}
	t.Cleanup(func() { proactiveTickHook = nil })
	reply, err := s.runProactiveTick("background")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestProactiveLockHold holds the same lock for a fixed time so the Java
// writer can prove it blocks on it. Run only when OQ_LOCK_HOLD_DIR is set.
func TestProactiveLockHold(t *testing.T) {
	dir := os.Getenv("OQ_LOCK_HOLD_DIR")
	if dir == "" {
		t.Skip("OQ_LOCK_HOLD_DIR not set")
	}
	signal := os.Getenv("OQ_LOCK_HOLD_SIGNAL")
	err := withProactivePolicyLock(filepath.Join(dir, "synthetic.sqlite"), func() error {
		if err := os.WriteFile(signal, []byte("held"), 0o600); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
