package codexauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// Public native-app client identifier used by the Codex OAuth flow, not a
	// client secret. The user still authorizes the account in their browser.
	ClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	AuthURL     = "https://auth.openai.com"
	RedirectURI = "http://localhost:1455/auth/callback"
)

type Client struct {
	http *http.Client
}

func NewClient(client *http.Client) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copy.Timeout = 30 * time.Second
	return &Client{http: &copy}
}

func (client *Client) Exchange(ctx context.Context, code, verifier, redirect string) (Credential, error) {
	if redirect != RedirectURI && redirect != AuthURL+"/deviceauth/callback" {
		return Credential{}, fmt.Errorf("invalid OAuth redirect URI")
	}
	return client.token(ctx, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {ClientID},
		"code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect},
	}, "")
}

func (client *Client) Refresh(ctx context.Context, previous Credential) (Credential, error) {
	credential, err := client.token(ctx, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {ClientID}, "refresh_token": {previous.RefreshToken},
	}, previous.RefreshToken)
	if err != nil {
		return Credential{}, err
	}
	if credential.AccountID != previous.AccountID {
		return Credential{}, fmt.Errorf("OAuth refresh returned a different account; log in again")
	}
	return credential, nil
}

func (client *Client) token(ctx context.Context, form url.Values, previousRefresh string) (Credential, error) {
	data, status, err := client.post(ctx, "/oauth/token", "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return Credential{}, err
	}
	if status != http.StatusOK {
		return Credential{}, fmt.Errorf("OpenAI Codex token exchange failed (HTTP %d); log in again if authorization expired", status)
	}
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if json.Unmarshal(data, &response) != nil || response.ExpiresIn <= 0 || response.ExpiresIn > 365*24*3600 {
		return Credential{}, fmt.Errorf("invalid OpenAI Codex token response")
	}
	if response.RefreshToken == "" {
		response.RefreshToken = previousRefresh
	}
	accountID, err := accountFromToken(response.AccessToken)
	if err != nil {
		return Credential{}, err
	}
	credential := Credential{Type: "oauth", AccessToken: response.AccessToken, RefreshToken: response.RefreshToken,
		AccountID: accountID, ExpiresAt: time.Now().UTC().Add(time.Duration(response.ExpiresIn) * time.Second)}
	return credential, credential.Validate()
}

// Only extracts routing metadata from the server-issued token. OpenAI verifies
// token authenticity when it receives the request; this is not local JWT auth.
func accountFromToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 32<<10 {
		return "", fmt.Errorf("OpenAI Codex access token has no account identity")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err != nil || json.Unmarshal(data, &claims) != nil || !validToken(claims.Auth.AccountID) {
		return "", fmt.Errorf("OpenAI Codex access token has no account identity")
	}
	return claims.Auth.AccountID, nil
}

func (client *Client) post(ctx context.Context, path, contentType string, body []byte) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, AuthURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("cannot construct OAuth request")
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "sn-runtime")
	response, err := client.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, fmt.Errorf("OpenAI Codex authentication network request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (128<<10)+1))
	if err != nil || len(data) > 128<<10 {
		return nil, response.StatusCode, fmt.Errorf("cannot read OpenAI Codex authentication response")
	}
	return data, response.StatusCode, nil
}

type DeviceCode struct {
	DeviceAuthID string      `json:"device_auth_id"`
	UserCode     string      `json:"user_code"`
	Interval     json.Number `json:"interval"`
}

func (client *Client) LoginDevice(ctx context.Context, notify func(url, code string) error) (Credential, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	data, status, err := client.post(ctx, "/api/accounts/deviceauth/usercode", "application/json", []byte(`{"client_id":"`+ClientID+`"}`))
	if err != nil {
		return Credential{}, err
	}
	if status != http.StatusOK {
		return Credential{}, fmt.Errorf("device-code login could not start (HTTP %d); use browser login or enable device-code login in ChatGPT settings", status)
	}
	var device DeviceCode
	if json.Unmarshal(data, &device) != nil || !validToken(device.DeviceAuthID) || !validToken(device.UserCode) {
		return Credential{}, fmt.Errorf("invalid device-code response")
	}
	seconds, err := device.Interval.Int64()
	if err != nil || seconds < 0 || seconds > 60 {
		return Credential{}, fmt.Errorf("invalid device-code polling interval")
	}
	if seconds < 1 {
		seconds = 1
	}
	if err := notify(AuthURL+"/codex/device", device.UserCode); err != nil {
		return Credential{}, err
	}
	payload, _ := json.Marshal(map[string]string{"device_auth_id": device.DeviceAuthID, "user_code": device.UserCode})
	for {
		if err := wait(ctx, time.Duration(seconds)*time.Second); err != nil {
			return Credential{}, err
		}
		data, status, err := client.post(ctx, "/api/accounts/deviceauth/token", "application/json", payload)
		if err != nil {
			return Credential{}, err
		}
		if status == http.StatusOK {
			var token struct {
				Code     string `json:"authorization_code"`
				Verifier string `json:"code_verifier"`
			}
			if json.Unmarshal(data, &token) != nil || !validToken(token.Code) || !validToken(token.Verifier) {
				return Credential{}, fmt.Errorf("invalid device authorization response")
			}
			return client.Exchange(ctx, token.Code, token.Verifier, AuthURL+"/deviceauth/callback")
		}
		code := deviceErrorCode(data)
		if code == "slow_down" {
			seconds = min(seconds+5, 60)
			continue
		}
		if status == 403 || status == 404 || code == "deviceauth_authorization_pending" {
			continue
		}
		return Credential{}, fmt.Errorf("device authorization failed (HTTP %d)", status)
	}
}

func deviceErrorCode(data []byte) string {
	var response struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &response) != nil {
		return ""
	}
	var code string
	if json.Unmarshal(response.Error, &code) == nil {
		return code
	}
	var object struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(response.Error, &object)
	return object.Code
}
