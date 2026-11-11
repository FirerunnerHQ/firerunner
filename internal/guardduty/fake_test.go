package guardduty

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/guardduty"
	"github.com/aws/aws-sdk-go-v2/service/guardduty/types"
)

// ownAccount is the account the fake's findings are in, AWS's documentation
// example; a test moves one to another.
const ownAccount = "123456789012"

// fakeGD is an in-memory GuardDuty: one detector's findings, listed by the
// criteria the package sends (it rejects any it does not know, so a typo in
// one fails a test rather than widening the list), a page of two at a time.
type fakeGD struct {
	mu        sync.Mutex
	detectors []string
	findings  []types.Finding
	err       error
	// ignoreCriteria lists every finding, as a criterion GuardDuty dropped
	// would: the package's own checks must hold without them.
	ignoreCriteria bool
	// criteria are the last ListFindings call's.
	criteria map[string]types.Condition
	// gets counts GetFindings calls; got the ids asked for.
	gets int
	got  []string
}

// add records a finding on an instance with the given tags; archived marks
// it suppressed.
func (f *fakeGD) add(id, typ string, sev float64, updated time.Time, archived bool, tags map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ts []types.Tag
	for k, v := range tags {
		ts = append(ts, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	f.findings = append(f.findings, types.Finding{
		AccountId: aws.String(ownAccount), Id: aws.String(id), Type: aws.String(typ), Severity: aws.Float64(sev),
		UpdatedAt: aws.String(updated.UTC().Format("2006-01-02T15:04:05.000Z")),
		Resource: &types.Resource{ResourceType: aws.String("Instance"), InstanceDetails: &types.InstanceDetails{
			InstanceId: aws.String("i-" + id), Tags: ts,
		}},
		Service: &types.Service{Archived: aws.Bool(archived), Count: aws.Int32(1)},
	})
}

// update makes finding id seen again at t, as GuardDuty does when the
// activity recurs.
func (f *fakeGD) update(id string, t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.findings {
		if aws.ToString(f.findings[i].Id) == id {
			f.findings[i].UpdatedAt = aws.String(t.UTC().Format("2006-01-02T15:04:05.000Z"))
			f.findings[i].Service.Count = aws.Int32(aws.ToInt32(f.findings[i].Service.Count) + 1)
		}
	}
}

func (f *fakeGD) ListDetectors(context.Context, *guardduty.ListDetectorsInput, ...func(*guardduty.Options)) (*guardduty.ListDetectorsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &guardduty.ListDetectorsOutput{DetectorIds: slices.Clone(f.detectors)}, nil
}

func (f *fakeGD) matches(fd types.Finding, c map[string]types.Condition) (bool, error) {
	tagKeys := func() []string {
		var ks []string
		for _, t := range fd.Resource.InstanceDetails.Tags {
			ks = append(ks, aws.ToString(t.Key))
		}
		return ks
	}
	updated, _ := time.Parse(time.RFC3339Nano, aws.ToString(fd.UpdatedAt))
	for field, cond := range c {
		switch field {
		case "resource.resourceType":
			if !slices.Contains(cond.Equals, aws.ToString(fd.Resource.ResourceType)) {
				return false, nil
			}
		case "resource.instanceDetails.tags.key":
			if !slices.ContainsFunc(cond.Equals, func(k string) bool { return slices.Contains(tagKeys(), k) }) {
				return false, nil
			}
		case "service.archived":
			if !slices.Contains(cond.Equals, strconv.FormatBool(aws.ToBool(fd.Service.Archived))) {
				return false, nil
			}
		case "updatedAt":
			if cond.GreaterThanOrEqual == nil || updated.UnixMilli() < *cond.GreaterThanOrEqual {
				return false, nil
			}
		case "accountId":
			if !slices.Contains(cond.Equals, aws.ToString(fd.AccountId)) {
				return false, nil
			}
		case "severity":
			if cond.GreaterThanOrEqual == nil || aws.ToFloat64(fd.Severity) < float64(*cond.GreaterThanOrEqual) {
				return false, nil
			}
		default:
			return false, fmt.Errorf("fake: unknown criterion %q", field)
		}
	}
	return true, nil
}

func (f *fakeGD) ListFindings(_ context.Context, in *guardduty.ListFindingsInput, _ ...func(*guardduty.Options)) (*guardduty.ListFindingsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.criteria = in.FindingCriteria.Criterion
	if !slices.Contains(f.detectors, aws.ToString(in.DetectorId)) {
		return nil, errors.New("BadRequestException: no such detector")
	}
	var ids []string
	for _, fd := range f.findings {
		ok, err := f.matches(fd, in.FindingCriteria.Criterion)
		if err != nil {
			return nil, err
		}
		if ok || f.ignoreCriteria {
			ids = append(ids, aws.ToString(fd.Id))
		}
	}
	start := 0
	if in.NextToken != nil {
		start, _ = strconv.Atoi(*in.NextToken)
	}
	end := min(start+2, len(ids))
	out := &guardduty.ListFindingsOutput{FindingIds: ids[start:end]}
	if end < len(ids) {
		out.NextToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

func (f *fakeGD) GetFindings(_ context.Context, in *guardduty.GetFindingsInput, _ ...func(*guardduty.Options)) (*guardduty.GetFindingsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if len(in.FindingIds) > 50 {
		return nil, errors.New("BadRequestException: at most 50 ids")
	}
	f.got = append(f.got, in.FindingIds...)
	out := &guardduty.GetFindingsOutput{}
	for _, fd := range f.findings {
		if slices.Contains(in.FindingIds, aws.ToString(fd.Id)) {
			out.Findings = append(out.Findings, fd)
		}
	}
	return out, nil
}
