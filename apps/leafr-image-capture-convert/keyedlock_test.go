package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func lockKeys(k *keyedLock) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.m)
}

func TestKeyedLockDropsIdleKeys(t *testing.T) {
	var k keyedLock
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		release, err := k.acquire(ctx, fmt.Sprintf("10.5.%d.%d:%d", i/256, i%256, 8080+i))
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if n := lockKeys(&k); n != 0 {
		t.Fatalf("idle keys retained after release: %d", n)
	}
}

func TestKeyedLockCancelledWaiterDropsKey(t *testing.T) {
	var k keyedLock
	release, err := k.acquire(context.Background(), "/dev/video0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := k.acquire(ctx, "/dev/video0"); err == nil {
		t.Fatal("expected source_busy error for a busy key")
	}
	release()
	if n := lockKeys(&k); n != 0 {
		t.Fatalf("keys retained after holder and waiter finished: %d", n)
	}
}

func TestKeyedLockSerialisesSameKey(t *testing.T) {
	var k keyedLock
	ctx := context.Background()
	release, err := k.acquire(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		r, err := k.acquire(ctx, "dev")
		if err != nil {
			t.Error(err)
			return
		}
		acquired <- r
	}()
	select {
	case <-acquired:
		t.Fatal("second acquire succeeded while the key was held")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case r := <-acquired:
		r()
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never acquired the released key")
	}
	if n := lockKeys(&k); n != 0 {
		t.Fatalf("keys retained after all holders released: %d", n)
	}
}
