package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"testing"

	"github.com/firerunnerhq/firerunner/internal/cacheserver"
)

// cache-server's listener lets this host's own connections past its caps:
// with more connections held from loopback than one guest may hold, `firerunner
// status` still reads /stats, and nothing is counted as refused. The caps
// themselves, for a guest's address, are tested in internal/jitserver.
func TestCacheListenLetsThisHostPastTheCaps(t *testing.T) {
	ln, err := cacheListen("127.0.0.1:0", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := cacheserver.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetConnRefusals(ln.Refused)
	hs := &http.Server{Handler: srv, ConnContext: srv.ConnectionContext}
	served := make(chan struct{})
	go func() { defer close(served); _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close(); <-served })

	// Idle connections, each held open by the server waiting for a request.
	// Accept takes them in order, so all are decided before the request below.
	for range cacheserver.MaxConnsPerSource + 1 {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + ln.Addr().String() + "/stats")
	if err != nil {
		t.Fatalf("/stats from this host with the per-source cap full: %v", err)
	}
	defer resp.Body.Close()
	var st cacheserver.Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.ConnRefused != 0 {
		t.Fatalf("this host's connections counted as %d refused (%v)", st.ConnRefused, st.ConnRefusedByCap)
	}
}
