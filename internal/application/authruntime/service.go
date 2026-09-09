// Package authruntime coordinates user-controlled login and local credential
// lifecycle. Profile loading and status checks never resolve or refresh tokens.
package authruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/yy003x/runtime/internal/infrastructure/codexauth"
	"github.com/yy003x/runtime/pkg/model"
)

type Service struct {
	store  codexauth.Store
	client *codexauth.Client
}

// Refresh on demand before a call, with enough margin for a long request.
// Status uses the same window but never performs network or storage writes.
const refreshWindow = 5 * time.Minute

var errCredentialsChanged = errors.New("credentials changed; read the current account and confirm again")

func New(store codexauth.Store, client *codexauth.Client) *Service {
	return &Service{store: store, client: client}
}

type Status struct {
	Provider           string     `json:"provider"`
	LoggedIn           bool       `json:"logged_in"`
	NeedsRefresh       bool       `json:"needs_refresh"`
	AccountFingerprint string     `json:"account_fingerprint,omitempty"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
}

func (service *Service) Status() (Status, error) {
	status, _, err := service.snapshot()
	return status, err
}

func (service *Service) snapshot() (Status, uint64, error) {
	document, err := service.store.Read()
	if err != nil {
		return Status{}, 0, err
	}
	return statusOf(document), document.Revision, nil
}

func statusOf(document codexauth.Document) Status {
	status := Status{Provider: codexauth.Provider}
	if credential, found := document.Providers[codexauth.Provider]; found {
		digest := sha256.Sum256([]byte(credential.AccountID))
		status.LoggedIn, status.ExpiresAt = true, &credential.ExpiresAt
		status.NeedsRefresh = !credential.ExpiresAt.After(time.Now().Add(refreshWindow))
		status.AccountFingerprint = fmt.Sprintf("%x", digest[:6])
	}
	return status
}

// Login does not hold a credential lock while waiting for a human. If another
// login, logout or refresh changes the cache meanwhile, the old flow cannot
// overwrite that newer state.
func (service *Service) Login(ctx context.Context, device bool, notify func(url, code string) error) (Status, error) {
	return service.login(ctx, device, notify, nil)
}

func (service *Service) login(ctx context.Context, device bool, notify func(url, code string) error, expected *uint64) (Status, error) {
	before, err := service.store.Read()
	if err != nil {
		return Status{}, err
	}
	if expected != nil && before.Revision != *expected {
		return Status{}, errCredentialsChanged
	}
	var credential codexauth.Credential
	if device {
		credential, err = service.client.LoginDevice(ctx, notify)
	} else {
		credential, err = service.client.LoginBrowser(ctx, func(url string) error { return notify(url, "") })
	}
	if err != nil {
		return Status{}, err
	}
	var status Status
	err = service.store.Update(ctx, func(document *codexauth.Document) (bool, error) {
		if document.Revision != before.Revision {
			return false, errCredentialsChanged
		}
		document.Providers[codexauth.Provider] = credential
		status = statusOf(*document)
		return true, nil
	})
	return status, err
}

// Logout removes only local credentials. It also advances the revision when
// already logged out so a previously started login cannot resurrect them.
func (service *Service) Logout(ctx context.Context) error {
	return service.logout(ctx, nil)
}

func (service *Service) logout(ctx context.Context, expected *uint64) error {
	return service.store.Update(ctx, func(document *codexauth.Document) (bool, error) {
		if expected != nil && document.Revision != *expected {
			return false, errCredentialsChanged
		}
		delete(document.Providers, codexauth.Provider)
		return true, nil
	})
}

func (service *Service) Resolve(ctx context.Context, driver model.DriverName, rejected string) (model.Credentials, error) {
	if driver != model.DriverOpenAICodexResponses {
		return model.Credentials{}, fmt.Errorf("unsupported OAuth driver")
	}
	document, err := service.store.Read()
	if err != nil {
		return model.Credentials{}, err
	}
	credential, found := document.Providers[codexauth.Provider]
	if !found {
		return model.Credentials{}, fmt.Errorf("OpenAI Codex is not logged in; run sn-cli auth login openai-codex")
	}
	needsRefresh := func(value codexauth.Credential) bool {
		return !value.ExpiresAt.After(time.Now().Add(refreshWindow)) || rejected != "" && rejected == value.AccessToken
	}
	if needsRefresh(credential) {
		err = service.store.Update(ctx, func(document *codexauth.Document) (bool, error) {
			current, found := document.Providers[codexauth.Provider]
			if !found {
				return false, fmt.Errorf("OpenAI Codex was logged out; log in again")
			}
			if current.AccountID != credential.AccountID {
				return false, fmt.Errorf("OpenAI Codex account changed during credential resolution; retry the call")
			}
			credential = current
			if !needsRefresh(current) {
				return false, nil
			}
			refreshed, err := service.client.Refresh(ctx, current)
			if err != nil {
				return false, err
			}
			credential = refreshed
			document.Providers[codexauth.Provider] = refreshed
			return true, nil
		})
		if err != nil {
			return model.Credentials{}, err
		}
	}
	return model.Credentials{AccessToken: credential.AccessToken, AccountID: credential.AccountID}, nil
}
