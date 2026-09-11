package fsutil

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWithFileLockSerialisesWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counter")
	lock := path + ".lock"
	os.WriteFile(path, []byte("0"), FileMode)

	// Twenty goroutines each read-modify-write the counter under the lock.
	// Without serialisation at least two reads see the same value and the
	// final count comes up short — the lost-update failure this exists for.
	var wg sync.WaitGroup
	var errs int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := WithFileLock(lock, 5*time.Second, func() error {
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				n := 0
				for _, c := range string(b) {
					if c >= '0' && c <= '9' {
						n = n*10 + int(c-'0')
					}
				}
				time.Sleep(time.Millisecond) // widen the race window
				return os.WriteFile(path, []byte(itoa(n+1)), FileMode)
			})
			if err != nil {
				atomic.AddInt32(&errs, 1)
			}
		}()
	}
	wg.Wait()
	if errs > 0 {
		t.Fatalf("%d writers failed", errs)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "20" {
		t.Fatalf("counter = %q, want 20 (lost updates)", b)
	}
}

func TestWithFileLockTimeout(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "busy.lock")
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		if err := WithFileLock(lock, time.Second, func() error {
			close(held)
			<-release
			return nil
		}); err != nil {
			t.Error(err)
		}
	}()
	<-held
	err := WithFileLock(lock, 20*time.Millisecond, func() error { return nil })
	close(release)
	if err == nil {
		t.Fatal("second locker succeeded while the lock was held")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
