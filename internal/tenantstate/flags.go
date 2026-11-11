package tenantstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// Flag says a tenant is flagged for review: a GuardDuty finding named one of
// its machines. A flag never suspends: an operator reviews it, then
// suspends the tenant (firerunner tenant suspend) or clears the flag
// (firerunner tenant clear-flag). The daemon keeps it until then.
type Flag struct {
	// Type, Tier, Instance and Finding are the latest finding's.
	Type     string `json:"type"`
	Tier     string `json:"tier,omitempty"`
	Instance string `json:"instance,omitempty"`
	Finding  string `json:"finding"`
	// Severity is the highest seen.
	Severity float64 `json:"severity"`
	// Count is how many findings, or updates of one, flagged it.
	Count int `json:"count"`
	// First and Last are when GuardDuty last updated the first and the
	// latest of them.
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// Flags are the flagged tenants, by id.
type Flags map[string]Flag

// IDs returns the flags' ids, sorted.
func (fl Flags) IDs() []string { return sortedKeys(fl) }

// Cursor is how far the daemon has read GuardDuty's findings, so that a
// restart neither flags a tenant twice for one finding nor misses one.
type Cursor struct {
	// Since is the oldest update time the next read asks for.
	Since time.Time `json:"since"`
	// Seen holds the update time of each finding flagged since Since, by
	// finding id: a read that lists one again flags nothing new.
	Seen map[string]time.Time `json:"seen,omitempty"`
}

// FlagStore keeps the flags, and the cursor of the reads that set them.
type FlagStore interface {
	LoadFlags(ctx context.Context) (Flags, Cursor, error)
	PutFlag(ctx context.Context, id string, f Flag) error
	DeleteFlag(ctx context.Context, id string) error
	PutCursor(ctx context.Context, c Cursor) error
	String() string
}

// cursorName is the cursor's key among the flags; no tenant id starts with
// an underscore but Install.
const cursorName = "_cursor"

// FlagsSpec is where flags are kept beside the states at spec: its name with
// -flags added, ssm:/fr/control/tenant-state-flags beside
// ssm:/fr/control/tenant-state, /var/lib/fr/tenant-state-flags.json beside
// /var/lib/fr/tenant-state.json. They are kept apart because the daemon
// writes flags, and only an operator writes states.
func FlagsSpec(spec string) string {
	if path, ok := strings.CutPrefix(spec, SSMPrefix); ok {
		return SSMPrefix + path + "-flags"
	}
	ext := filepath.Ext(spec)
	return strings.TrimSuffix(spec, ext) + "-flags" + ext
}

// OpenFlags returns the flag store beside the state store spec names.
func OpenFlags(spec string, newSSM func() (SSMAPI, error)) (FlagStore, error) {
	if err := CheckSpec(spec); err != nil {
		return nil, err
	}
	spec = FlagsSpec(spec)
	if path, ok := strings.CutPrefix(spec, SSMPrefix); ok {
		api, err := newSSM()
		if err != nil {
			return nil, err
		}
		return &SSMFlagStore{API: api, Path: path}, nil
	}
	return &FileFlagStore{Path: spec}, nil
}

// flagFile is a FileFlagStore's content.
type flagFile struct {
	Cursor Cursor `json:"cursor"`
	Flags  Flags  `json:"flags"`
}

// FileFlagStore keeps the flags and the cursor in one JSON file. A missing
// file holds neither.
type FileFlagStore struct{ Path string }

func (f *FileFlagStore) String() string { return f.Path }

func (f *FileFlagStore) load() (flagFile, error) {
	ff := flagFile{Flags: Flags{}}
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return ff, nil
	}
	if err != nil {
		return ff, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return ff, fmt.Errorf("%s: %w", f.Path, errEmpty)
	}
	if err := json.Unmarshal(b, &ff); err != nil {
		return ff, fmt.Errorf("%s: %w", f.Path, err)
	}
	if ff.Flags == nil {
		ff.Flags = Flags{}
	}
	return ff, nil
}

// LoadFlags reads the file.
func (f *FileFlagStore) LoadFlags(context.Context) (Flags, Cursor, error) {
	ff, err := f.load()
	if err != nil {
		return nil, Cursor{}, err
	}
	return ff.Flags, ff.Cursor, nil
}

// PutFlag records a flag.
func (f *FileFlagStore) PutFlag(_ context.Context, id string, fl Flag) error {
	return f.update(func(ff *flagFile) { ff.Flags[id] = fl })
}

// DeleteFlag clears a flag.
func (f *FileFlagStore) DeleteFlag(_ context.Context, id string) error {
	return f.update(func(ff *flagFile) { delete(ff.Flags, id) })
}

// PutCursor records the cursor.
func (f *FileFlagStore) PutCursor(_ context.Context, c Cursor) error {
	return f.update(func(ff *flagFile) { ff.Cursor = c })
}

func (f *FileFlagStore) update(change func(*flagFile)) error {
	return rewrite(f.Path, func() ([]byte, error) {
		ff, err := f.load()
		if errors.Is(err, errEmpty) {
			ff, err = flagFile{Flags: Flags{}}, nil
		}
		if err != nil {
			return nil, err
		}
		change(&ff)
		return json.MarshalIndent(ff, "", "  ")
	})
}

// SSMFlagStore keeps each flag as a String parameter <Path>/<id> holding its
// JSON, and the cursor as <Path>/_cursor. The daemon writes a flag only when
// a finding adds to it, so an operator's clear and its writes seldom meet.
type SSMFlagStore struct {
	API  SSMAPI
	Path string
}

func (s *SSMFlagStore) String() string { return SSMPrefix + s.Path }

// LoadFlags reads every parameter under the path. A flag that does not parse
// still flags its tenant: someone wrote it.
func (s *SSMFlagStore) LoadFlags(ctx context.Context) (Flags, Cursor, error) {
	fl, cur := Flags{}, Cursor{}
	p := ssm.NewGetParametersByPathPaginator(s.API, &ssm.GetParametersByPathInput{Path: aws.String(s.Path)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, Cursor{}, err
		}
		for _, prm := range page.Parameters {
			id, val := strings.TrimPrefix(aws.ToString(prm.Name), s.Path+"/"), []byte(aws.ToString(prm.Value))
			if id == cursorName {
				// A cursor that does not parse reads as none: the
				// next read looks back as far as a first one.
				_ = json.Unmarshal(val, &cur)
				continue
			}
			var f Flag
			if err := json.Unmarshal(val, &f); err != nil {
				at := aws.ToTime(prm.LastModifiedDate)
				f = Flag{Type: "its flag record does not parse: " + err.Error(), First: at, Last: at}
			}
			fl[id] = f
		}
	}
	return fl, cur, nil
}

// PutFlag writes the tenant's flag.
func (s *SSMFlagStore) PutFlag(ctx context.Context, id string, f Flag) error {
	return s.put(ctx, id, f, "firerunner: this tenant is flagged for review (firerunner tenant clear-flag clears it)")
}

// DeleteFlag removes the tenant's flag; one already gone is no error.
func (s *SSMFlagStore) DeleteFlag(ctx context.Context, id string) error {
	_, err := s.API.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(s.Path + "/" + id)})
	var nf *types.ParameterNotFound
	if errors.As(err, &nf) {
		return nil
	}
	return err
}

// PutCursor writes the cursor.
func (s *SSMFlagStore) PutCursor(ctx context.Context, c Cursor) error {
	return s.put(ctx, cursorName, c, "firerunner: how far the daemon has read GuardDuty findings")
}

func (s *SSMFlagStore) put(ctx context.Context, name string, v any, desc string) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.API.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String(s.Path + "/" + name), Value: aws.String(string(b)),
		Type: types.ParameterTypeString, Overwrite: aws.Bool(true), Description: aws.String(desc),
	})
	return err
}
