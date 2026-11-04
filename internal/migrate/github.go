package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitHub is the REST client migrate needs, authenticated as the person
// running it: their token, not the runner App's, which stays read-only (#146).
type GitHub struct {
	Base   string // https://api.github.com
	Token  string
	Client *http.Client
}

// APIError is a non-2xx GitHub response.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// NeedsWorkflowScope reports a refusal to change .github/workflows for want
// of the token's workflow scope.
func NeedsWorkflowScope(err error) bool {
	var e *APIError
	return errors.As(err, &e) && strings.Contains(e.Message, "`workflow` scope")
}

func notFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (g *GitHub) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(g.Base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+g.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := g.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &m)
		return &APIError{method, path, resp.StatusCode, m.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
