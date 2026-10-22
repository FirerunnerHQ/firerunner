package ec2

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAMIResolver_ImageID(t *testing.T) {
	f := newFakeEC2()
	r := newAMIResolver(f, nil)
	img, err := r.resolve(context.Background(), "ami-0abc")
	if err != nil {
		t.Fatal(err)
	}
	if img.id != "ami-0abc" || img.rootDevice != "/dev/sda1" {
		t.Fatalf("image = %+v", img)
	}
}

func TestAMIResolver_SSMParameterAndCache(t *testing.T) {
	f := newFakeEC2()
	s := &fakeSSM{params: map[string]string{"/firerunner/ami": "ami-0abc"}}
	r := newAMIResolver(f, s)
	now := time.Unix(0, 0)
	r.now = func() time.Time { return now }

	for _, ref := range []string{"/firerunner/ami", "resolve:ssm:/firerunner/ami"} {
		img, err := r.resolve(context.Background(), ref)
		if err != nil || img.id != "ami-0abc" {
			t.Fatalf("resolve(%q) = %+v, %v", ref, img, err)
		}
	}
	calls := s.calls
	// A second resolve inside the TTL is served from the cache.
	if _, err := r.resolve(context.Background(), "/firerunner/ami"); err != nil {
		t.Fatal(err)
	}
	if s.calls != calls {
		t.Fatalf("SSM called %d times, want the cached answer", s.calls-calls)
	}
	// A rebuilt image published to the parameter is picked up after the TTL.
	s.params["/firerunner/ami"] = "ami-0new"
	f.images["ami-0new"] = "/dev/xvda"
	now = now.Add(amiCacheTTL)
	img, err := r.resolve(context.Background(), "/firerunner/ami")
	if err != nil || img.id != "ami-0new" || img.rootDevice != "/dev/xvda" {
		t.Fatalf("after TTL = %+v, %v, want the rebuilt image", img, err)
	}
}

func TestAMIResolver_Errors(t *testing.T) {
	cases := map[string]struct {
		ref    string
		ssm    ParamAPI
		wantIn string
	}{
		"unknown image":       {"ami-0missing", nil, "not found"},
		"no ssm client":       {"/firerunner/ami", nil, "no SSM client"},
		"missing parameter":   {"/nope", &fakeSSM{params: map[string]string{}}, "ParameterNotFound"},
		"parameter not an id": {"/bad", &fakeSSM{params: map[string]string{"/bad": "ubuntu-24.04"}}, "does not hold an image ID"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newAMIResolver(newFakeEC2(), c.ssm)
			_, err := r.resolve(context.Background(), c.ref)
			if err == nil || !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("err = %v, want it to mention %q", err, c.wantIn)
			}
		})
	}
	// A failure is not cached: the next launch retries the lookup.
	s := &fakeSSM{params: map[string]string{}}
	r := newAMIResolver(newFakeEC2(), s)
	_, _ = r.resolve(context.Background(), "/later")
	s.params["/later"] = "ami-0abc"
	if _, err := r.resolve(context.Background(), "/later"); err != nil {
		t.Fatalf("resolve after the parameter appeared: %v", err)
	}
}
