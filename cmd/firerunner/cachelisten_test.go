package main

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/cacheserver"
)

// cache-server's listener caps one address's connections: past
// cacheserver.MaxConnsPerSource, a connection is closed, never served.
func TestCacheListenCapsOneSource(t *testing.T) {
	ln, err := cacheListen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var accepted []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range accepted {
			_ = c.Close()
		}
	})
	held := 0
	deadline := time.Now().Add(10 * time.Second)
	for held < cacheserver.MaxConnsPerSource {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		held++
	}
	// Wait until the server has taken all of them, then one more.
	for {
		mu.Lock()
		n := len(accepted)
		mu.Unlock()
		if n == cacheserver.MaxConnsPerSource {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("accepted %d of %d", n, cacheserver.MaxConnsPerSource)
		}
		time.Sleep(5 * time.Millisecond)
	}
	over, err := net.Dial("tcp", ln.Addr().String())
	if err == nil {
		defer over.Close()
		_ = over.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, err := over.Read(make([]byte, 1)); n != 0 || err == nil {
			t.Fatalf("a connection over the cap read %d bytes, err %v; want it closed", n, err)
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("a connection over the cap was left open")
		}
	}
	mu.Lock()
	n := len(accepted)
	mu.Unlock()
	if n != cacheserver.MaxConnsPerSource {
		t.Fatalf("the listener handed over %d connections, want %d", n, cacheserver.MaxConnsPerSource)
	}
}
