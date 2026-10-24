//go:build linux

package db_lib

import (
	"bytes"
	"golang.org/x/sys/unix"
	"os/exec"
	"runtime"
	"sync"
	"testing"
)

func TestRCDAnsiblePTYAllocation(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p, s, err := openAnsiblePTY()
				if err != nil {
					t.Error(err)
					return
				}
				locked, err := unix.IoctlGetInt(int(p.Fd()), unix.TIOCGPTLCK)
				if err != nil || locked != 0 {
					t.Errorf("PTY locked: %v %v", locked, err)
				}
				p.Close()
				s.Close()
				if j%10 == 0 {
					runtime.GC()
				}
			}
		}()
	}
	wg.Wait()
}

func TestRCDAnsiblePTYStartBoundary(t *testing.T) {
	cmd := exec.Command("/this-executable-does-not-exist")
	p, err := startAnsiblePTY(cmd)
	if err == nil || p != nil || cmd.Process != nil {
		t.Fatal("failed start gained process", err)
	}
	cmd = exec.Command("/bin/sh", "-c", "printf started; exit 7")
	var out bytes.Buffer
	cmd.Stdout = &out
	p, err = startAnsiblePTY(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = cmd.Wait(); err == nil || cmd.Process == nil || out.String() != "started" {
		t.Fatal("exit failure is not launch failure", err, out.String())
	}
}
