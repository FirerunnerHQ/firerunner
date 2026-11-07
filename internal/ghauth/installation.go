package ghauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Installation hands out one installation's tokens, narrowed to Permissions,
// a new one each time: a revoked token must not outlive its revocation, and
// the caller cannot say when GitHub refused one. Ask only as often as a token
// is needed, as the scale set client does once per Actions service session.
type Installation struct {
	// Base is the REST API root, e.g. https://api.github.com.
	Base        string
	App         App
	Permissions map[string]string
	HTTP        *http.Client
}

// Token returns a new installation token.
func (i *Installation) Token(ctx context.Context) (string, error) {
	if !i.App.Valid() {
		return "", errors.New("ghauth: no GitHub App installation")
	}
	jwt, err := i.App.JWT.Token(ctx)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{"permissions": i.Permissions})
	if err != nil {
		return "", err
	}
	path := fmt.Sprintf("%s/app/installations/%d/access_tokens", i.Base, i.App.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := i.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ghauth: installation token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if resp.StatusCode == http.StatusUnauthorized {
			i.App.JWT.Refused(jwt)
		}
		return "", fmt.Errorf("ghauth: installation %d token: HTTP %d %s", i.App.InstallationID, resp.StatusCode, e.Message)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("ghauth: installation token: %w", err)
	}
	if out.Token == "" || !out.ExpiresAt.After(time.Now()) {
		return "", errors.New("ghauth: installation token without a usable expiry")
	}
	return out.Token, nil
}
