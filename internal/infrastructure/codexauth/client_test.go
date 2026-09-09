package codexauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func tokenFor(account string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"`+account+`"}}`)) + ".fixture"
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestExchangeRefreshAndPrivateFailures(t *testing.T) {
	for _, operation := range []string{"exchange", "refresh", "missing_rotated_token", "changed_account", "bad_token", "denied", "redirect"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			client := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.String() != AuthURL+"/oauth/token" {
					t.Fatal("wrong token endpoint")
				}
				if err := request.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if request.Form.Get("client_id") != ClientID {
					t.Fatal("wrong client")
				}
				if operation == "exchange" && (request.Form.Get("grant_type") != "authorization_code" || request.Form.Get("code_verifier") != "verifier") {
					t.Fatal("wrong PKCE form")
				}
				if operation == "refresh" && (request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "fixture-refresh") {
					t.Fatal("wrong refresh form")
				}
				if operation == "denied" {
					return jsonResponse(401, `{"error":"fixture-secret"}`), nil
				}
				if operation == "redirect" {
					response := jsonResponse(302, `{}`)
					response.Header.Set("Location", "https://untrusted.invalid/")
					return response, nil
				}
				account := "fixture-account"
				if operation == "changed_account" {
					account = "another-account"
				}
				access, refresh := tokenFor(account), "rotated-refresh"
				if operation == "bad_token" {
					access = "fixture-secret"
				}
				if operation == "missing_rotated_token" {
					refresh = ""
				}
				body, _ := json.Marshal(map[string]any{"access_token": access, "refresh_token": refresh, "expires_in": 3600})
				return jsonResponse(200, string(body)), nil
			})})
			var credential Credential
			var err error
			if operation == "exchange" {
				credential, err = client.Exchange(context.Background(), "code", "verifier", RedirectURI)
			} else {
				credential, err = client.Refresh(context.Background(), fixtureCredential())
			}
			good := operation == "exchange" || operation == "refresh" || operation == "missing_rotated_token"
			if good && (err != nil || credential.AccountID != "fixture-account" || credential.RefreshToken == "") {
				t.Fatalf("successful token flow failed: %v", err)
			}
			if !good && (err == nil || strings.Contains(err.Error(), "fixture-secret")) {
				t.Fatalf("unsafe error=%v", err)
			}
			if calls != 1 {
				t.Fatalf("requests=%d", calls)
			}
		})
	}
}

func TestBrowserLoginChecksStateAndPKCE(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var challenge string
	client := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(request.Form.Get("code_verifier")))
		if challenge != base64.RawURLEncoding.EncodeToString(digest[:]) || request.Form.Get("code") != "fixture-code" {
			t.Fatal("PKCE mismatch")
		}
		return jsonResponse(200, fmt.Sprintf(`{"access_token":%q,"refresh_token":"refresh","expires_in":3600}`, tokenFor("fixture-account"))), nil
	})})
	credential, err := client.loginBrowser(context.Background(), listener, func(authorize string) error {
		parsed, err := url.Parse(authorize)
		if err != nil {
			return err
		}
		query := parsed.Query()
		challenge = query.Get("code_challenge")
		if query.Get("code_challenge_method") != "S256" || query.Get("redirect_uri") != RedirectURI || query.Get("originator") != "sn-cli" {
			t.Fatal("bad authorize URL")
		}
		for _, state := range []string{"incorrect", query.Get("state")} {
			response, err := http.Get("http://" + listener.Addr().String() + "/auth/callback?" + url.Values{"state": {state}, "code": {"fixture-code"}}.Encode())
			if err != nil {
				return err
			}
			response.Body.Close()
			if state == "incorrect" && response.StatusCode != 400 {
				t.Fatal("state was not validated")
			}
			if state != "incorrect" && response.StatusCode != 200 {
				t.Fatal("valid callback rejected")
			}
		}
		return nil
	})
	if err != nil || credential.AccountID != "fixture-account" {
		t.Fatalf("login: %v", err)
	}
}

func TestDeviceLogin(t *testing.T) {
	stage := 0
	client := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		stage++
		switch stage {
		case 1:
			if request.URL.Path != "/api/accounts/deviceauth/usercode" {
				t.Fatal("wrong start path")
			}
			return jsonResponse(200, `{"device_auth_id":"device","user_code":"CODE-123","interval":"0"}`), nil
		case 2:
			if request.URL.Path != "/api/accounts/deviceauth/token" {
				t.Fatal("wrong poll path")
			}
			return jsonResponse(403, `{}`), nil
		case 3:
			return jsonResponse(200, `{"authorization_code":"code","code_verifier":"verifier"}`), nil
		case 4:
			request.ParseForm()
			if request.Form.Get("redirect_uri") != AuthURL+"/deviceauth/callback" {
				t.Fatal("wrong device redirect")
			}
			return jsonResponse(200, fmt.Sprintf(`{"access_token":%q,"refresh_token":"refresh","expires_in":3600}`, tokenFor("fixture-account"))), nil
		}
		t.Fatal("unexpected request")
		return nil, nil
	})})
	credential, err := client.LoginDevice(context.Background(), func(uri, code string) error {
		if uri != AuthURL+"/codex/device" || code != "CODE-123" {
			t.Fatal("bad device instructions")
		}
		return nil
	})
	if err != nil || stage != 4 || credential.AccountID != "fixture-account" {
		t.Fatalf("device stages=%d err=%v", stage, err)
	}
}

func TestBrowserCancellationClosesListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("cancelled login made a token request")
		return nil, fmt.Errorf("unexpected")
	})})
	_, err = client.loginBrowser(ctx, listener, func(string) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	connection, err := net.Dial("tcp4", address)
	if err == nil {
		connection.Close()
		t.Fatal("callback listener survived cancellation")
	}
}
