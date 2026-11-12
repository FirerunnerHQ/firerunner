package entitlement

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/firerunnerhq/firerunner/internal/tenantstate"
)

// pathSSM serves fixed parameters under one path.
type pathSSM struct {
	tenantstate.SSMAPI
	params []types.Parameter
}

func (p pathSSM) GetParametersByPath(context.Context, *ssm.GetParametersByPathInput, ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	return &ssm.GetParametersByPathOutput{Parameters: p.params}, nil
}

// An SSM store reads each tenant's parameter, with when it was last written,
// so status can say how old billing's word is.
func TestSSMStoreLoadsWithTheWriteTime(t *testing.T) {
	written := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	s := &SSMStore{Path: "/fr/control/entitlements", API: pathSSM{params: []types.Parameter{{
		Name:             aws.String("/fr/control/entitlements/acme"),
		Value:            aws.String(`{"account_state":"active","budget_state":"ok","valid_until":"2026-12-01T00:00:00Z","limits":{"max_runners":2}}`),
		LastModifiedDate: aws.Time(written),
	}}}}
	rs, err := s.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, ok := rs["acme"]
	if !ok || r.Invalid != "" || !r.Written.Equal(written) || r.PlanLimits().MaxRunners != 2 {
		t.Fatalf("loaded %+v", rs)
	}
}
