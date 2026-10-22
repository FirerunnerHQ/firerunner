// Package ec2 launches one ephemeral EC2 instance per GitHub Actions job.
//
// It implements provisioner.Provisioner alongside the Firecracker microVM
// provisioner: Launch starts an instance for one JIT runner and blocks until
// that instance is gone. The instance gets its JIT config from the control
// VM's jitserver over the VPC; user-data names only the endpoint.
package ec2

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// API is the subset of the EC2 client this package calls, so tests can
// substitute a fake.
type API interface {
	RunInstances(context.Context, *ec2.RunInstancesInput, ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeImages(context.Context, *ec2.DescribeImagesInput, ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
}

// ParamAPI is the subset of the SSM client used to resolve an AMI parameter.
type ParamAPI interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// Tag keys on every instance and volume this package creates. The reaper finds
// an install's leftovers by TagOwner.
const (
	TagTier   = "firerunner:tier"
	TagRunner = "firerunner:runner"
	TagOwner  = "firerunner:owner"
)
