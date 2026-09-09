package codexauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

func (client *Client) LoginBrowser(ctx context.Context, notify func(string) error) (Credential, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:1455")
	if err != nil {
		return Credential{}, fmt.Errorf("cannot listen on 127.0.0.1:1455; close another login flow or use --device-code")
	}
	return client.loginBrowser(ctx, listener, notify)
}

func (client *Client) loginBrowser(ctx context.Context, listener net.Listener, notify func(string) error) (Credential, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	defer listener.Close()
	verifier, state := rand.Text()+rand.Text(), rand.Text()
	digest := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"response_type": {"code"}, "client_id": {ClientID}, "redirect_uri": {RedirectURI},
		"scope": {"openid profile email offline_access"}, "state": {state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}, "code_challenge_method": {"S256"},
		"id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"}, "originator": {"sn-cli"},
	}
	type callback struct {
		code string
		err  error
	}
	received := make(chan callback, 1)
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 8 << 10,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if request.Method != http.MethodGet || request.URL.Path != "/auth/callback" {
				http.Error(w, "Not found", http.StatusNotFound)
				return
			}
			values := request.URL.Query()
			if len(values["state"]) != 1 || subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(state)) != 1 {
				http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
				return
			}
			response := callback{code: values.Get("code")}
			if values.Get("error") != "" {
				response.err = fmt.Errorf("OpenAI Codex authorization was denied or cancelled")
			} else if len(values["code"]) != 1 || !validToken(response.code) {
				http.Error(w, "Invalid authorization code", http.StatusBadRequest)
				return
			}
			select {
			case received <- response:
				_, _ = io.WriteString(w, "Authorization returned. Go back to your terminal to check the login result.\n")
			default:
				http.Error(w, "Authorization already received", http.StatusConflict)
			}
		}),
	}
	defer server.Close()
	serverError := make(chan error, 1)
	go func() { serverError <- server.Serve(listener) }()
	if err := notify(AuthURL + "/oauth/authorize?" + query.Encode()); err != nil {
		return Credential{}, err
	}
	select {
	case <-ctx.Done():
		return Credential{}, ctx.Err()
	case <-serverError:
		return Credential{}, fmt.Errorf("OAuth callback listener stopped")
	case response := <-received:
		if response.err != nil {
			return Credential{}, response.err
		}
		return client.Exchange(ctx, response.code, verifier, RedirectURI)
	}
}
