package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"

	"github.com/firerunnerhq/firerunner/internal/tenantstate"
)

// deniedSSM reads one flag and refuses every write, as a control VM's role
// refuses an operator's act.
type deniedSSM struct{ code string }

func (deniedSSM) GetParametersByPath(context.Context, *awsssm.GetParametersByPathInput, ...func(*awsssm.Options)) (*awsssm.GetParametersByPathOutput, error) {
	return &awsssm.GetParametersByPathOutput{Parameters: []ssmtypes.Parameter{{
		Name: aws.String("/fr/control/tenant-state-flags/acme"), Value: aws.String(`{"reason":"x"}`),
	}}}, nil
}

func (d deniedSSM) PutParameter(context.Context, *awsssm.PutParameterInput, ...func(*awsssm.Options)) (*awsssm.PutParameterOutput, error) {
	return nil, &smithy.GenericAPIError{Code: d.code, Message: "not authorized"}
}

func (d deniedSSM) DeleteParameter(context.Context, *awsssm.DeleteParameterInput, ...func(*awsssm.Options)) (*awsssm.DeleteParameterOutput, error) {
	return nil, &smithy.GenericAPIError{Code: d.code, Message: "not authorized"}
}

func runTenantSSM(api deniedSSM, args ...string) (int, string) {
	var out, errOut bytes.Buffer
	code := tenantCmd(args, &out, &errOut, tenantOpeners{
		states: func(string) (tenantstate.Store, error) {
			return &tenantstate.SSMStore{API: api, Path: "/fr/control/tenant-state"}, nil
		},
		flags: func(string) (tenantstate.FlagStore, error) {
			return &tenantstate.SSMFlagStore{API: api, Path: "/fr/control/tenant-state-flags"}, nil
		},
	})
	return code, errOut.String()
}

// Where AWS refuses a write (on a control VM, whose role only reads tenant
// state and writes flags), the command says so and prints the command to run
// where an operator's credentials are.
func TestTenantWriteDeniedSaysWhereToRunIt(t *testing.T) {
	clearFREnv(t)
	const spec = "ssm:/fr/control/tenant-state"
	for _, args := range [][]string{
		{"clear-flag", "acme"},
		{"suspend", "acme", "--reason", "abuse report"},
		{"resume", "acme"},
	} {
		code, errOut := runTenantSSM(deniedSSM{code: "AccessDeniedException"}, append(args, "--state", spec, "--env-file", "none")...)
		want := "firerunner tenant " + args[0] + " acme --state " + spec
		if code != 1 || !strings.Contains(errOut, "operator credentials") || !strings.Contains(errOut, want) {
			t.Errorf("%s denied: exit %d\n%s", args[0], code, errOut)
		}
	}
	// Another failure is reported as it is, with no advice about credentials.
	code, errOut := runTenantSSM(deniedSSM{code: "ThrottlingException"}, "clear-flag", "acme", "--state", spec, "--env-file", "none")
	if code != 1 || strings.Contains(errOut, "operator credentials") || !strings.Contains(errOut, "ThrottlingException") {
		t.Errorf("throttled: exit %d\n%s", code, errOut)
	}
}
