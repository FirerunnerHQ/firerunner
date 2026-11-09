// Package awscreds hands short-lived AWS credentials from one process to
// another through a file.
//
// The daemon assumes a narrow role and writes its session credentials to a
// file (Refresher); a process that must not reach instance metadata, such as
// cache-server, reads them from that file (FileProvider). The file has the
// shape of the AWS credential_process output, so tools that know it can read
// it too.
package awscreds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// FileMode is the credentials file's mode: the writer's group (the reader's
// user) may read it, nobody else.
const FileMode os.FileMode = 0o640

// ExpiryWindow is how long before the file's credentials expire that a reader
// reads the file again. The refresher rewrites the file far earlier, so a
// reader always finds credentials that outlive this window.
const ExpiryWindow = 5 * time.Minute

// Source names this provider in aws.Credentials.Source.
const Source = "firerunner-credentials-file"

// fileCreds is the credential_process JSON shape.
type fileCreds struct {
	Version         int
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string
	SessionToken    string     `json:",omitempty"`
	Expiration      *time.Time `json:",omitempty"`
}

// Write replaces path with c, atomically: a reader sees the old file or the
// new one, never a part. The file's mode is FileMode.
func Write(path string, c aws.Credentials) error {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return errors.New("awscreds: credentials without a key")
	}
	fc := fileCreds{Version: 1, AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken}
	if c.CanExpire {
		exp := c.Expires.UTC()
		fc.Expiration = &exp
	}
	data, err := json.Marshal(fc)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("awscreds: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed
	if err := f.Chmod(FileMode); err != nil {
		f.Close()
		return fmt.Errorf("awscreds: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("awscreds: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("awscreds: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("awscreds: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("awscreds: %w", err)
	}
	return nil
}

// FileProvider is an aws.CredentialsProvider that reads the file Write
// writes. Wrap it in NewFileCache, so it is read again only near expiry.
type FileProvider struct {
	Path string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Retrieve reads the file. Credentials already expired are an error, so a
// request fails with the reason rather than with S3's ExpiredToken.
func (p FileProvider) Retrieve(context.Context) (aws.Credentials, error) {
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("awscreds: %w", err)
	}
	var fc fileCreds
	if err := json.Unmarshal(data, &fc); err != nil {
		return aws.Credentials{}, fmt.Errorf("awscreds: %s: %w", p.Path, err)
	}
	if fc.Version != 1 || fc.AccessKeyID == "" || fc.SecretAccessKey == "" {
		return aws.Credentials{}, fmt.Errorf("awscreds: %s: want Version 1 with AccessKeyId and SecretAccessKey", p.Path)
	}
	c := aws.Credentials{AccessKeyID: fc.AccessKeyID, SecretAccessKey: fc.SecretAccessKey, SessionToken: fc.SessionToken, Source: Source}
	if fc.Expiration != nil {
		now := time.Now
		if p.Now != nil {
			now = p.Now
		}
		if !now().Before(*fc.Expiration) {
			return aws.Credentials{}, fmt.Errorf("awscreds: %s: credentials expired at %s", p.Path, fc.Expiration.Format(time.RFC3339))
		}
		c.CanExpire, c.Expires = true, *fc.Expiration
	}
	return c, nil
}

// NewFileCache is the provider for an SDK client: the file, read again
// ExpiryWindow before its credentials expire, or after a failed read.
func NewFileCache(path string) *aws.CredentialsCache {
	return aws.NewCredentialsCache(FileProvider{Path: path}, func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow = ExpiryWindow
	})
}

// Wait returns once path holds credentials FileProvider accepts, polling
// every interval, or with ctx's error. It logs while it waits, so a writer
// that never comes is visible.
func Wait(ctx context.Context, path string, interval time.Duration, log *slog.Logger) error {
	p := FileProvider{Path: path}
	logged := time.Time{}
	for {
		_, err := p.Retrieve(ctx)
		if err == nil {
			return nil
		}
		if time.Since(logged) >= time.Minute {
			log.Warn("waiting for AWS credentials", "file", path, "err", err)
			logged = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// STSAPI is the subset of the STS client the refresher calls, so tests can
// substitute a fake.
type STSAPI interface {
	AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

// Defaults for a Refresher's zero fields.
const (
	DefaultSessionDuration = time.Hour
	DefaultRefreshEvery    = 15 * time.Minute
	DefaultMinBackoff      = 5 * time.Second
	DefaultMaxBackoff      = time.Minute
	DefaultSessionName     = "firerunner-cache"
)

// Refresher assumes RoleARN and keeps Path holding its session credentials,
// rewriting them every RefreshEvery, long before they expire.
type Refresher struct {
	API         STSAPI
	RoleARN     string
	Path        string
	SessionName string
	// SessionDuration is the session's lifetime; RefreshEvery how often it is
	// replaced. RefreshEvery must leave the session more than ExpiryWindow.
	SessionDuration time.Duration
	RefreshEvery    time.Duration
	// MinBackoff and MaxBackoff bound the wait after a failed refresh.
	MinBackoff, MaxBackoff time.Duration
	Log                    *slog.Logger

	mu     sync.Mutex
	status Status
}

// Status is what a Refresher has done, for status and doctor.
type Status struct {
	File string
	// LastRefresh is when the file was last rewritten, and Expires when the
	// credentials in it expire; both zero before the first success.
	LastRefresh time.Time
	Expires     time.Time
	// LastError is the latest failed refresh's, at LastErrorAt; it stays
	// after a later success, which LastRefresh then shows.
	LastError   string
	LastErrorAt time.Time
}

// Status returns a copy of the refresher's state.
func (r *Refresher) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	s.File = r.Path
	return s
}

func (r *Refresher) record(exp time.Time, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	if err != nil {
		r.status.LastError, r.status.LastErrorAt = err.Error(), now
		return
	}
	r.status.LastRefresh, r.status.Expires = now, exp.UTC()
}

func (r *Refresher) defaults() {
	if r.SessionName == "" {
		r.SessionName = DefaultSessionName
	}
	if r.SessionDuration == 0 {
		r.SessionDuration = DefaultSessionDuration
	}
	if r.RefreshEvery == 0 {
		r.RefreshEvery = DefaultRefreshEvery
	}
	if r.MinBackoff == 0 {
		r.MinBackoff = DefaultMinBackoff
	}
	if r.MaxBackoff == 0 {
		r.MaxBackoff = DefaultMaxBackoff
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
}

// Refresh assumes the role once and writes the credentials, recording the
// outcome in Status.
func (r *Refresher) Refresh(ctx context.Context) (time.Time, error) {
	exp, err := r.refresh(ctx)
	r.record(exp, err)
	return exp, err
}

func (r *Refresher) refresh(ctx context.Context) (time.Time, error) {
	r.defaults()
	out, err := r.API.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(r.RoleARN),
		RoleSessionName: aws.String(r.SessionName),
		DurationSeconds: aws.Int32(int32(r.SessionDuration / time.Second)),
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("assume %s: %w", r.RoleARN, err)
	}
	c := out.Credentials
	if c == nil || c.AccessKeyId == nil || c.SecretAccessKey == nil || c.Expiration == nil {
		return time.Time{}, fmt.Errorf("assume %s: no credentials in the response", r.RoleARN)
	}
	err = Write(r.Path, aws.Credentials{
		AccessKeyID: *c.AccessKeyId, SecretAccessKey: *c.SecretAccessKey, SessionToken: aws.ToString(c.SessionToken),
		CanExpire: true, Expires: *c.Expiration,
	})
	return *c.Expiration, err
}

// Run refreshes at once, then every RefreshEvery, until ctx ends. A failed
// refresh is logged and retried with backoff; the file keeps the previous
// credentials, valid for most of a session, meanwhile.
func (r *Refresher) Run(ctx context.Context) {
	r.defaults()
	backoff := r.MinBackoff
	for {
		wait := r.RefreshEvery
		exp, err := r.Refresh(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			r.Log.Warn("cache credentials refresh failed; retrying", "role", r.RoleARN, "file", r.Path, "retry_in", backoff, "err", err)
			wait = backoff
			backoff = min(backoff*2, r.MaxBackoff)
		default:
			r.Log.Info("cache credentials refreshed", "role", r.RoleARN, "file", r.Path, "expires", exp.Format(time.RFC3339))
			backoff = r.MinBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
