package ghauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestInstallationTokenNarrowedAndReused(t *testing.T) {
	k := key(t)
	var calls atomic.Int32
	var clock atomic.Int64 // the test's time, which the server issues by
	clock.Store(time.Now().Unix())
	var asked map[string]map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/7/access_tokens" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		verify(t, r.Header.Get("Authorization")[len("Bearer "):], &k.PublicKey)
		_ = json.NewDecoder(r.Body).Decode(&asked)
		n := calls.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":%q}`, n, time.Unix(clock.Load(), 0).Add(time.Hour).Format(time.RFC3339))
	}))
	defer srv.Close()
	i := &Installation{Base: srv.URL, App: App{InstallationID: 7, JWT: NewJWT("Iv1", KeySigner{Key: k})},
		Permissions: map[string]string{"organization_self_hosted_runners": "write"}, now: func() time.Time { return time.Unix(clock.Load(), 0) }}
	for range 2 {
		if tok, err := i.Token(context.Background()); err != nil || tok != "ghs_1" {
			t.Fatalf("token %q, %v", tok, err)
		}
	}
	if fmt.Sprint(asked) != "map[permissions:map[organization_self_hosted_runners:write]]" {
		t.Fatalf("asked for %v", asked)
	}
	clock.Add(int64((56 * time.Minute).Seconds()))
	if tok, _ := i.Token(context.Background()); tok != "ghs_2" {
		t.Fatalf("a token five minutes from expiry was reused: %q", tok)
	}
}

func TestInstallationTokenRefusalDropsTheJWT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"bad JWT"}`))
	}))
	defer srv.Close()
	s := &countingSigner{Signer: KeySigner{Key: key(t)}}
	clock := time.Unix(1_000_000, 0)
	j := NewJWT("Iv1", s).WithClock(func() time.Time { return clock })
	i := &Installation{Base: srv.URL, App: App{InstallationID: 7, JWT: j}}
	if _, err := i.Token(context.Background()); err == nil {
		t.Fatal("no error on 401")
	}
	clock = clock.Add(time.Minute)
	_, _ = i.Token(context.Background())
	_, _ = i.Token(context.Background())
	if s.n != 2 {
		t.Fatalf("%d signatures, want a new JWT after a refusal a minute old", s.n)
	}
}
