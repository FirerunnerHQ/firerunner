// Package tenantstate keeps whether each tenant an install serves is
// suspended, and why. An operator's command writes it (firerunner tenant
// suspend), and the daemon reads it again every few seconds, so a suspension
// reaches a running daemon, and outlives a restart or a replaced control VM.
//
// Only suspensions are kept: a tenant with no record is active. The store is
// a JSON file on the host, or, for a deployment configured from SSM, one SSM
// parameter per suspended tenant under a path.
package tenantstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// Install names the install's own tenant, whose id is otherwise empty. A
// tenant id never starts with an underscore, so it names no catalog tenant.
const Install = "_install"

// Record is one tenant's suspension.
type Record struct {
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// States are the suspended tenants, by id.
type States map[string]Record

// Store keeps the states.
type Store interface {
	Load(ctx context.Context) (States, error)
	Put(ctx context.Context, id string, r Record) error
	Delete(ctx context.Context, id string) error
	// String says where it keeps them.
	String() string
}

var tenantID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidID reports whether id names a tenant: a catalog id, or Install.
func ValidID(id string) bool { return id == Install || tenantID.MatchString(id) }

// SSMPrefix starts a store spec that names an SSM path.
const SSMPrefix = "ssm:"

// CheckSpec reports whether spec names a store: "ssm:" and an absolute SSM
// path, or an absolute file path.
func CheckSpec(spec string) error {
	if path, ok := strings.CutPrefix(spec, SSMPrefix); ok {
		if !strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") || strings.Contains(path, "//") {
			return fmt.Errorf("%q: an SSM path is absolute, e.g. ssm:/firerunner/control/tenant-state", spec)
		}
		return nil
	}
	if !filepath.IsAbs(spec) {
		return fmt.Errorf("%q: give an absolute file path, or ssm: and an SSM path", spec)
	}
	return nil
}

// SSMAPI is the part of the SSM client a store uses.
type SSMAPI interface {
	GetParametersByPath(context.Context, *ssm.GetParametersByPathInput, ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
	PutParameter(context.Context, *ssm.PutParameterInput, ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	DeleteParameter(context.Context, *ssm.DeleteParameterInput, ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
}

// Open returns the store spec names. newSSM is asked for a client only for an
// SSM path.
func Open(spec string, newSSM func() (SSMAPI, error)) (Store, error) {
	if err := CheckSpec(spec); err != nil {
		return nil, err
	}
	if path, ok := strings.CutPrefix(spec, SSMPrefix); ok {
		api, err := newSSM()
		if err != nil {
			return nil, err
		}
		return &SSMStore{API: api, Path: path}, nil
	}
	return &FileStore{Path: spec}, nil
}

// FileStore keeps the states in one JSON file, {"<id>": {"reason", "since"}}.
// A missing file holds no suspension.
type FileStore struct{ Path string }

func (f *FileStore) String() string { return f.Path }

// Load reads the file.
func (f *FileStore) Load(context.Context) (States, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return States{}, nil
	}
	if err != nil {
		return nil, err
	}
	st := States{}
	if len(strings.TrimSpace(string(b))) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	return st, nil
}

// Put records a suspension, replacing the file whole.
func (f *FileStore) Put(ctx context.Context, id string, r Record) error {
	return f.update(ctx, func(st States) { st[id] = r })
}

// Delete removes a suspension.
func (f *FileStore) Delete(ctx context.Context, id string) error {
	return f.update(ctx, func(st States) { delete(st, id) })
}

// update rewrites the file with change applied, by rename, so the daemon
// never reads half of it. It is readable by all: it holds no secret, and the
// daemon may run as another user.
func (f *FileStore) update(ctx context.Context, change func(States)) error {
	st, err := f.Load(ctx)
	if err != nil {
		return err
	}
	change(st)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".tenant-state-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.Path)
}

// SSMStore keeps each suspension as a String parameter <Path>/<id> holding
// its record's JSON. Writing one tenant never touches another's, and the
// daemon reads them all with one call per ten.
type SSMStore struct {
	API  SSMAPI
	Path string
}

func (s *SSMStore) String() string { return SSMPrefix + s.Path }

// Load reads every parameter under the path. A record that does not parse
// still suspends its tenant: someone wrote it.
func (s *SSMStore) Load(ctx context.Context) (States, error) {
	st := States{}
	p := ssm.NewGetParametersByPathPaginator(s.API, &ssm.GetParametersByPathInput{Path: aws.String(s.Path)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, prm := range page.Parameters {
			id := strings.TrimPrefix(aws.ToString(prm.Name), s.Path+"/")
			var r Record
			if err := json.Unmarshal([]byte(aws.ToString(prm.Value)), &r); err != nil {
				r = Record{Reason: "its suspension record does not parse: " + err.Error(), Since: aws.ToTime(prm.LastModifiedDate)}
			}
			st[id] = r
		}
	}
	return st, nil
}

// Put writes the tenant's parameter.
func (s *SSMStore) Put(ctx context.Context, id string, r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.API.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String(s.Path + "/" + id), Value: aws.String(string(b)),
		Type: types.ParameterTypeString, Overwrite: aws.Bool(true),
		Description: aws.String("firerunner: this tenant is suspended (firerunner tenant resume lifts it)"),
	})
	return err
}

// Delete removes the tenant's parameter; one already gone is no error.
func (s *SSMStore) Delete(ctx context.Context, id string) error {
	_, err := s.API.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(s.Path + "/" + id)})
	var nf *types.ParameterNotFound
	if errors.As(err, &nf) {
		return nil
	}
	return err
}

// IDs returns the states' ids, sorted.
func (st States) IDs() []string {
	ids := make([]string, 0, len(st))
	for id := range st {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
