package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestResolveViaDoHServers(t *testing.T) {
	t.Parallel()

	want := netip.AddrFrom4([4]byte{93, 184, 216, 34})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != dohContentType {
			t.Errorf("Content-Type = %q, want %q", ct, dohContentType)
		}
		w.Header().Set("Content-Type", dohContentType)
		_, _ = w.Write(packAResponse(t, "example.com.", [4]byte{93, 184, 216, 34}))
	}))
	defer srv.Close()

	got, err := resolveViaDoHServers(context.Background(), []string{srv.URL}, "example.com")
	if err != nil {
		t.Fatalf("resolveViaDoHServers() error: %v", err)
	}
	if got != want {
		t.Fatalf("resolveViaDoHServers() = %v, want %v", got, want)
	}
}

func TestResolveViaDoHServersFallsThrough(t *testing.T) {
	t.Parallel()

	// First endpoint errors; the second answers. Confirms the loop advances past
	// a failing server instead of giving up.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", dohContentType)
		_, _ = w.Write(packAResponse(t, "example.com.", [4]byte{1, 2, 3, 4}))
	}))
	defer good.Close()

	got, err := resolveViaDoHServers(context.Background(), []string{bad.URL, good.URL}, "example.com")
	if err != nil {
		t.Fatalf("resolveViaDoHServers() error: %v", err)
	}
	if want := netip.AddrFrom4([4]byte{1, 2, 3, 4}); got != want {
		t.Fatalf("resolveViaDoHServers() = %v, want %v", got, want)
	}
}

func TestResolveViaDoHServersNoServers(t *testing.T) {
	t.Parallel()

	if _, err := resolveViaDoHServers(context.Background(), nil, "example.com"); err == nil {
		t.Fatal("resolveViaDoHServers() with no servers returned nil error, want error")
	}
}

// packAResponse builds a minimal DNS response carrying a single A record.
func packAResponse(t *testing.T, name string, ip [4]byte) []byte {
	t.Helper()
	dnsName, err := dnsmessage.NewName(name)
	if err != nil {
		t.Fatalf("NewName: %v", err)
	}
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true},
		Answers: []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{
					Name:  dnsName,
					Type:  dnsmessage.TypeA,
					Class: dnsmessage.ClassINET,
				},
				Body: &dnsmessage.AResource{A: ip},
			},
		},
	}
	b, err := msg.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return b
}
