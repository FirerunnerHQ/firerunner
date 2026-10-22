package ec2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// amiCacheTTL bounds how long a resolved image is reused. A rebuilt AMI
// published to the SSM parameter is picked up within this window, without a
// lookup on every launch.
const amiCacheTTL = 5 * time.Minute

// image is a resolved AMI.
type image struct {
	id         string
	rootDevice string
}

// amiResolver turns a tier's ami setting (an image ID or an SSM parameter
// name) into an image ID and root device name, caching the answer.
type amiResolver struct {
	ec2 API
	ssm ParamAPI
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cachedImage
}

type cachedImage struct {
	img     image
	expires time.Time
}

func newAMIResolver(api API, params ParamAPI) *amiResolver {
	return &amiResolver{ec2: api, ssm: params, now: time.Now, cache: map[string]cachedImage{}}
}

// resolve returns the image for ref. An "ami-" ref is used as is; anything
// else names an SSM parameter (with or without a "resolve:ssm:" prefix) whose
// value is an image ID.
func (r *amiResolver) resolve(ctx context.Context, ref string) (image, error) {
	r.mu.Lock()
	if c, ok := r.cache[ref]; ok && r.now().Before(c.expires) {
		r.mu.Unlock()
		return c.img, nil
	}
	r.mu.Unlock()

	id := ref
	if !strings.HasPrefix(ref, "ami-") {
		if r.ssm == nil {
			return image{}, fmt.Errorf("ami %q names an SSM parameter but no SSM client is configured", ref)
		}
		name := strings.TrimPrefix(ref, "resolve:ssm:")
		out, err := r.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
		if err != nil {
			return image{}, fmt.Errorf("resolve ami parameter %q: %w", name, err)
		}
		if out.Parameter == nil || !strings.HasPrefix(aws.ToString(out.Parameter.Value), "ami-") {
			return image{}, fmt.Errorf("ami parameter %q does not hold an image ID", name)
		}
		id = aws.ToString(out.Parameter.Value)
	}
	out, err := r.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{id}})
	if err != nil {
		return image{}, fmt.Errorf("describe image %s: %w", id, err)
	}
	if len(out.Images) != 1 {
		return image{}, fmt.Errorf("image %s not found", id)
	}
	img := image{id: id, rootDevice: aws.ToString(out.Images[0].RootDeviceName)}
	if img.rootDevice == "" {
		return image{}, errors.New("image " + id + " has no root device name")
	}
	r.mu.Lock()
	r.cache[ref] = cachedImage{img: img, expires: r.now().Add(amiCacheTTL)}
	r.mu.Unlock()
	return img, nil
}
