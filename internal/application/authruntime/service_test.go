package authruntime

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/infrastructure/codexauth"
	"github.com/yy003x/runtime/pkg/model"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (fn transportFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func seed(t *testing.T, store codexauth.Store, expired bool) {
	t.Helper()
	expiry := time.Now().Add(time.Hour)
	if expired {
		expiry = time.Now().Add(-time.Hour)
	}
	if err := store.Update(context.Background(), func(document *codexauth.Document) (bool, error) {
		document.Providers[codexauth.Provider] = codexauth.Credential{
			Type: "oauth", AccessToken: "fixture-old-token", RefreshToken: "fixture-refresh",
			AccountID: "fixture-account", ExpiresAt: expiry,
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentRefreshAndConditional401Refresh(t *testing.T) {
	for _, rejected := range []string{"", "fixture-old-token"} {
		t.Run(rejected, func(t *testing.T) {
			store := codexauth.Store{StateDir: filepath.Join(t.TempDir(), "state")}
			seed(t, store, rejected == "")
			var calls atomic.Int32
			access := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account"}}`)) + ".new"
			client := codexauth.NewClient(&http.Client{Transport: transportFunc(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				request.ParseForm()
				if request.Form.Get("refresh_token") != "fixture-refresh" {
					t.Error("rotated token refreshed again")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"access_token":%q,"refresh_token":"rotated-refresh","expires_in":3600}`, access)))}, nil
			})})
			var workers sync.WaitGroup
			for i := 0; i < 16; i++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					// Separate service objects exercise the process-independent file lock.
					credential, err := New(store, client).Resolve(context.Background(), model.DriverOpenAICodexResponses, rejected)
					if err != nil || credential.AccessToken != access {
						t.Errorf("resolve: %v", err)
					}
				}()
			}
			workers.Wait()
			if calls.Load() != 1 {
				t.Fatalf("refresh requests=%d", calls.Load())
			}
			document, err := store.Read()
			if err != nil || document.Providers[codexauth.Provider].RefreshToken != "rotated-refresh" {
				t.Fatalf("rotated credential not persisted: %v", err)
			}
		})
	}
}

func TestAutomaticRefreshWindowAndCachedReuse(t *testing.T) {
	for _, test := range []struct {
		name     string
		validFor time.Duration
		refresh  bool
	}{
		{"valid", time.Hour, false},
		{"outside_window", 6 * time.Minute, false},
		{"inside_window", 4 * time.Minute, true},
		{"expired", -time.Minute, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := codexauth.Store{StateDir: filepath.Join(t.TempDir(), "state")}
			if err := store.Update(context.Background(), func(document *codexauth.Document) (bool, error) {
				document.Providers[codexauth.Provider] = codexauth.Credential{
					Type: "oauth", AccessToken: "fixture-old-token", RefreshToken: "fixture-refresh",
					AccountID: "fixture-account", ExpiresAt: time.Now().Add(test.validFor),
				}
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			access := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account"}}`)) + ".rotated"
			client := codexauth.NewClient(&http.Client{Transport: transportFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"access_token":%q,"refresh_token":"fixture-rotated-refresh","expires_in":3600}`, access)))}, nil
			})})
			service := New(store, client)
			before, err := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			status, err := service.Status()
			if err != nil || !status.LoggedIn || status.NeedsRefresh != test.refresh || calls != 0 {
				t.Fatalf("status/refresh window mismatch: status=%+v calls=%d err=%v", status, calls, err)
			}
			afterStatus, err := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
			if err != nil || string(before) != string(afterStatus) {
				t.Fatal("status wrote credentials")
			}
			wantToken, wantCalls := "fixture-old-token", 0
			if test.refresh {
				wantToken, wantCalls = access, 1
			}
			for i := 0; i < 2; i++ {
				// A newly composed service must reuse the persisted rotated token.
				credential, err := New(store, client).Resolve(context.Background(), model.DriverOpenAICodexResponses, "")
				if err != nil || credential.AccessToken != wantToken || calls != wantCalls {
					t.Fatalf("resolve %d: calls=%d err=%v", i, calls, err)
				}
			}
			status, err = service.Status()
			if err != nil || !status.LoggedIn || status.NeedsRefresh {
				t.Fatalf("refreshed status=%+v err=%v", status, err)
			}
		})
	}
}

func TestStatusIsReadOnlyAndLogoutDoesNotReviveCredentials(t *testing.T) {
	store := codexauth.Store{StateDir: filepath.Join(t.TempDir(), "state")}
	client := codexauth.NewClient(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected network request")
		return nil, fmt.Errorf("unexpected")
	})})
	service := New(store, client)
	status, err := service.Status()
	if err != nil || status.LoggedIn {
		t.Fatalf("status: %v", err)
	}
	if _, err := os.Stat(store.StateDir); !os.IsNotExist(err) {
		t.Fatal("status created state")
	}
	seed(t, store, true)
	status, err = service.Status()
	if err != nil || !status.LoggedIn || !status.NeedsRefresh || status.AccountFingerprint == "fixture-account" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if err := service.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(context.Background(), model.DriverOpenAICodexResponses, "fixture-old-token"); err == nil {
		t.Fatal("logout did not remove credentials")
	}
	data, _ := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
	if strings.Contains(string(data), "fixture-") {
		t.Fatal("logout retained credentials")
	}
}

func TestRefreshFailurePreservesCredentials(t *testing.T) {
	store := codexauth.Store{StateDir: filepath.Join(t.TempDir(), "state")}
	seed(t, store, true)
	before, _ := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
	service := New(store, codexauth.NewClient(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"fixture-refresh"}`))}, nil
	})}))
	_, err := service.Resolve(context.Background(), model.DriverOpenAICodexResponses, "")
	if err == nil || strings.Contains(err.Error(), "fixture-refresh") {
		t.Fatalf("err=%v", err)
	}
	after, _ := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
	if string(before) != string(after) {
		t.Fatal("failed refresh changed credentials")
	}
}
