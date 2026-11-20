package jira_test

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

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/jira"
)

func TestOAuthRotatesTheRefreshToken(t *testing.T) {
	refreshes := 0
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["grant_type"] != "refresh_token" || in["client_id"] != "app" ||
			in["client_secret"] != "app-secret" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		want := "r" + string(rune('0'+refreshes))
		if in["refresh_token"] != want {
			w.WriteHeader(http.StatusForbidden) // a retired token is refused
			return
		}
		refreshes++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a" + want,
			"refresh_token": "r" + string(rune('0'+refreshes)), "expires_in": 3600})
	}))
	defer tokens.Close()
	file := filepath.Join(t.TempDir(), "refresh")
	if err := os.WriteFile(file, []byte("r0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := newClock()
	o := &jira.OAuth{TokenURL: tokens.URL, ClientID: "app",
		ClientSecret: core.NewSecret("app-secret"),
		RefreshFile:  file, Now: clk.Now}
	header := func() string {
		req, _ := http.NewRequest(http.MethodGet, "https://x.test", nil)
		if err := o.Apply(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return req.Header.Get("Authorization")
	}
	if h := header(); h != "Bearer ar0" || header() != h || refreshes != 1 {
		t.Fatalf("header %q after %d refreshes", h, refreshes)
	}
	saved, _ := os.ReadFile(file)
	if strings.TrimSpace(string(saved)) != "r1" {
		t.Fatalf("refresh file = %q", saved)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("refresh file mode %v", st.Mode())
	}
	clk.Advance(2 * time.Hour)
	if h := header(); h != "Bearer ar1" || refreshes != 2 {
		t.Fatalf("after expiry: %q, %d", h, refreshes)
	}
	// A refused refresh token needs a person, not a retry.
	_ = os.WriteFile(file, []byte("stale"), 0o600)
	clk.Advance(2 * time.Hour)
	req, _ := http.NewRequest(http.MethodGet, "https://x.test", nil)
	if err := o.Apply(context.Background(), req); !core.IsKind(err, core.KindAuthRequired) {
		t.Fatalf("stale refresh token: %v", err)
	}
}
