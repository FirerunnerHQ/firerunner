package ec2

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
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
			}, CapacityFailsFast)
			_, err := client.RunInstances(context.Background(), &ec2.RunInstancesInput{
				MinCount: aws.Int32(1), MaxCount: aws.Int32(1), InstanceType: types.InstanceTypeM8aLarge,
			})
			if !isCode(err, c.code) {
				t.Fatalf("err = %v", err)
			}
			if got := attempts.Load(); got != c.attempts {
				t.Fatalf("%d attempts, want %d", got, c.attempts)
			}
		})
	}
}
