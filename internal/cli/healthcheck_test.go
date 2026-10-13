package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthURL(t *testing.T) {
	tests := map[string]string{
		"0.0.0.0:8080":   "http://127.0.0.1:8080/healthz",
		":4318":          "http://127.0.0.1:4318/healthz",
		"[::]:8080":      "http://127.0.0.1:8080/healthz",
		"10.1.2.3:9000":  "http://10.1.2.3:9000/healthz",
		"localhost:8080": "http://localhost:8080/healthz",
	}
	for in, want := range tests {
		got, err := healthURL(in)
		if err != nil || got != want {
			t.Errorf("healthURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := healthURL("nope"); err == nil {
		t.Error("expected error for address without port")
	}
}

func TestProbe(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer ok.Close()
	if err := probe(context.Background(), ok.URL); err != nil {
		t.Errorf("200 should pass: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	if err := probe(context.Background(), bad.URL); err == nil {
		t.Error("503 should fail")
	}

	bad.Close()
	if err := probe(context.Background(), bad.URL); err == nil {
		t.Error("connection refused should fail")
	}
}
