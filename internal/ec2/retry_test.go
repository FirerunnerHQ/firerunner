package ec2

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// A real SDK client against a server answering as EC2 does: no spot capacity is
// HTTP 500, and only that must not be retried.
func TestCapacityFailsFast(t *testing.T) {
	for _, c := range []struct {
		code     string
		attempts int32
	}{
		{"InsufficientInstanceCapacity", 1},
		{"InternalError", 3},
	} {
		t.Run(c.code, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, "<Response><Errors><Error><Code>%s</Code><Message>no</Message></Error></Errors><RequestID>r</RequestID></Response>", c.code)
			}))
			defer srv.Close()
			client := ec2.New(ec2.Options{
				Region:       "us-east-1",
				BaseEndpoint: aws.String(srv.URL),
				Credentials:  credentials.NewStaticCredentialsProvider("id", "secret", ""),
				Retryer:      retry.NewStandard(),
			})
			_, err := client.RunInstances(context.Background(), &ec2.RunInstancesInput{
				MinCount: aws.Int32(1), MaxCount: aws.Int32(1), InstanceType: types.InstanceTypeM8aLarge,
			}, capacityFailsFast)
			if !isCode(err, c.code) {
				t.Fatalf("err = %v", err)
			}
			if got := attempts.Load(); got != c.attempts {
				t.Fatalf("%d attempts, want %d", got, c.attempts)
			}
		})
	}
}

// The launch path must pass the option, and it must wrap the retryer the config
// chose, not replace it.
func TestLaunch_FailsFastOnCapacity(t *testing.T) {
	h := newHarness(t, nil)
	errc := h.launch(context.Background())
	inst := h.instance(t)
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	waitErr(t, errc)
	h.f.mu.Lock()
	opts := h.f.runOpts
	h.f.mu.Unlock()
	o := ec2.Options{Retryer: retry.NewAdaptiveMode()}
	for _, fn := range opts {
		fn(&o)
	}
	if o.Retryer.IsErrorRetryable(apiErr("InsufficientInstanceCapacity")) {
		t.Fatal("a launch retries a capacity error")
	}
	wrapped, ok := o.Retryer.(capacityRetryer)
	if !ok {
		t.Fatalf("retryer = %T", o.Retryer)
	}
	if _, adaptive := wrapped.RetryerV2.(*retry.AdaptiveMode); !adaptive {
		t.Fatalf("the configured retryer was replaced: %T", wrapped.RetryerV2)
	}
}
