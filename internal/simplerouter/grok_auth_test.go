package simplerouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadGrokCLISessionTokenUsesValidSession(t *testing.T) {
	home := withTestHome(t)
	authDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	entries := map[string]grokAuthEntry{
		"https://auth.x.ai::client": {
			Key:          "  fresh-token  ",
			ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
			OIDCIssuer:   "https://auth.x.ai",
			OIDCClientID: "client",
			RefreshToken: "refresh",
		},
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadGrokCLISessionToken(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if got != "fresh-token" {
		t.Fatalf("token = %q", got)
	}
}

func TestLoadGrokCLISessionTokenRefreshesExpiredSession(t *testing.T) {
	home := withTestHome(t)
	authDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}

	refreshCalls := 0
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		refreshCalls++
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_id") != "client" {
			t.Errorf("unexpected form: %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer issuer.Close()

	entries := map[string]grokAuthEntry{
		issuer.URL + "::client": {
			Key:          "expired-token",
			ExpiresAt:    time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
			OIDCIssuer:   issuer.URL,
			OIDCClientID: "client",
			RefreshToken: "old-refresh",
		},
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(authDir, "auth.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadGrokCLISessionToken(context.Background(), issuer.Client())
	if err != nil {
		t.Fatal(err)
	}
	if got != "new-access" {
		t.Fatalf("token = %q", got)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d", refreshCalls)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "new-access") || !strings.Contains(string(saved), "new-refresh") {
		t.Fatalf("auth.json not updated: %s", saved)
	}
}

func TestGrokSessionExpired(t *testing.T) {
	if grokSessionExpired("") {
		t.Fatal("empty expiry should not force refresh")
	}
	if grokSessionExpired(time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)) {
		t.Fatal("future expiry should be valid")
	}
	if !grokSessionExpired(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)) {
		t.Fatal("past expiry should be expired")
	}
}

func TestPickGrokAuthEntryPrefersFreshest(t *testing.T) {
	entries := map[string]grokAuthEntry{
		"old": {
			Key:       "old-token",
			ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		},
		"new": {
			Key:       "new-token",
			ExpiresAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano),
		},
		"empty": {Key: ""},
	}
	token, key, _, ok := pickGrokAuthEntry(entries)
	if !ok || token != "new-token" || key != "new" {
		t.Fatalf("got token=%q key=%q ok=%v", token, key, ok)
	}
}
