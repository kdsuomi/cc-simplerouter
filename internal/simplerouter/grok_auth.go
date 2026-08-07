package simplerouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Grok CLI stores OIDC session credentials in ~/.grok/auth.json after
// `grok login`. SimpleRouter reuses that session when no XAI_API_KEY is set,
// matching Grok Build's credential precedence for interactive machines.
//
// See https://docs.x.ai/build/enterprise#authentication

const (
	grokAuthDirName     = ".grok"
	grokAuthFileName    = "auth.json"
	grokSessionSkew     = 60 * time.Second
	grokRefreshTimeout  = 20 * time.Second
	defaultGrokOIDCPath = "/oauth2/token"
)

type grokAuthEntry struct {
	Key          string `json:"key"`
	AuthMode     string `json:"auth_mode"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	OIDCIssuer   string `json:"oidc_issuer"`
	OIDCClientID string `json:"oidc_client_id"`
}

// loadGrokCLISessionToken returns a usable bearer token from the Grok CLI auth
// file. It refreshes the OIDC access token when expired and writes the update
// back so the CLI and SimpleRouter stay in sync.
func loadGrokCLISessionToken(ctx context.Context, httpClient *http.Client) (string, error) {
	path, err := grokAuthPath()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}

	// auth.json is a map of "issuer::client_id" → session objects.
	var entries map[string]grokAuthEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	key, entryKey, entry, ok := pickGrokAuthEntry(entries)
	if !ok {
		return "", nil
	}
	if !grokSessionExpired(entry.ExpiresAt) {
		return key, nil
	}
	if strings.TrimSpace(entry.RefreshToken) == "" || strings.TrimSpace(entry.OIDCIssuer) == "" || strings.TrimSpace(entry.OIDCClientID) == "" {
		// Expired and not refreshable: treat as absent so the launcher can
		// fall through to a prompt.
		return "", nil
	}
	refreshed, err := refreshGrokOIDCToken(ctx, httpClient, entry)
	if err != nil {
		return "", fmt.Errorf("refresh Grok CLI session: %w", err)
	}
	entries[entryKey] = refreshed
	if err := writeGrokAuthFile(path, entries); err != nil {
		// Token is still usable even if we cannot rewrite the file.
		return cleanAPIKey(refreshed.Key), nil
	}
	return cleanAPIKey(refreshed.Key), nil
}

func pickGrokAuthEntry(entries map[string]grokAuthEntry) (token, mapKey string, entry grokAuthEntry, ok bool) {
	var bestKey string
	var best grokAuthEntry
	var bestExpiry time.Time
	found := false
	for k, e := range entries {
		tok := cleanAPIKey(e.Key)
		if tok == "" {
			continue
		}
		// Prefer the freshest usable session (longest remaining lifetime).
		exp, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(e.ExpiresAt))
		if !found || exp.After(bestExpiry) {
			bestKey = k
			best = e
			bestExpiry = exp
			found = true
		}
	}
	if !found {
		return "", "", grokAuthEntry{}, false
	}
	return cleanAPIKey(best.Key), bestKey, best, true
}

func grokSessionExpired(expiresAt string) bool {
	expiresAt = strings.TrimSpace(expiresAt)
	if expiresAt == "" {
		// No expiry metadata: use the token and let the API reject it.
		return false
	}
	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		exp, err = time.Parse(time.RFC3339, expiresAt)
	}
	if err != nil {
		return false
	}
	return !time.Now().Before(exp.Add(-grokSessionSkew))
}

func refreshGrokOIDCToken(ctx context.Context, httpClient *http.Client, entry grokAuthEntry) (grokAuthEntry, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	issuer := strings.TrimRight(strings.TrimSpace(entry.OIDCIssuer), "/")
	tokenURL := issuer + defaultGrokOIDCPath
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", entry.RefreshToken)
	form.Set("client_id", entry.OIDCClientID)

	ctx, cancel := context.WithTimeout(ctx, grokRefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return grokAuthEntry{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return grokAuthEntry{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return grokAuthEntry{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return grokAuthEntry{}, fmt.Errorf("OIDC refresh HTTP %d: %s", resp.StatusCode, truncateForError(string(body), 200))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return grokAuthEntry{}, fmt.Errorf("decode OIDC refresh: %w", err)
	}
	access := cleanAPIKey(out.AccessToken)
	if access == "" {
		return grokAuthEntry{}, errors.New("OIDC refresh returned empty access_token")
	}
	updated := entry
	updated.Key = access
	if rt := cleanAPIKey(out.RefreshToken); rt != "" {
		updated.RefreshToken = rt
	}
	if out.ExpiresIn > 0 {
		updated.ExpiresAt = time.Now().UTC().Add(time.Duration(out.ExpiresIn) * time.Second).Format(time.RFC3339Nano)
	}
	return updated, nil
}

func writeGrokAuthFile(path string, entries map[string]grokAuthEntry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func grokAuthPath() (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, grokAuthDirName, grokAuthFileName), nil
}

func truncateForError(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
