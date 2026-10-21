package ownership

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLockProcessHelper(t *testing.T) {
	path := os.Getenv("AEGIS_TEST_LOCK_PATH")
	if path == "" {
		return
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fmt.Println("locked")
	for {
		time.Sleep(time.Second)
	}
}

func TestLockReleasedAfterProcessKill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockProcessHelper$")
	cmd.Env = append(os.Environ(), "AEGIS_TEST_LOCK_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("child lock: %q %v", line, err)
	}
	if lock, err := Acquire(path); err == nil {
		lock.Close()
		t.Fatal("second process acquired live lock")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child did not terminate abruptly")
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal("crash retained lock:", err)
	}
	lock.Close()
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), ".aegis-monitor.lock")); err != nil {
		t.Fatal("lock inode was unlinked")
	}
}

func TestLockAliasesAndDifferentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if file, err := Acquire(alias); err == nil {
		file.Close()
		t.Fatal("database symlink bypassed lock")
	}
	other, err := Acquire(filepath.Join(t.TempDir(), "different.db"))
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
}
