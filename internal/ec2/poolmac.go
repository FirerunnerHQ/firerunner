package ec2

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// An instance's tags are all a restarted firerunner knows of it, and anyone in
// the account who may tag an instance can write them. So what firerunner acts
// on after a restart is signed with an HMAC under a key only the control plane
// holds, bound to the instance ID so that a tag copied to another instance
// fails:
//
//   - TagPoolMAC: a pool instance's owner and tier, tagged as soon as
//     RunInstances names it. The pool takes in an instance only with a valid
//     one: one without is never registered for a JIT config nor started.
//   - TagPoolDiskMAC: the image a pool instance was launched from and the EBS
//     volumes it was launched with, tagged while it first warms. A claim
//     starts it only while its image and volumes still match.
//   - TagRunnerMAC: the runner on a job instance, tagged wherever TagRunner
//     is. The reaper deregisters a runner at GitHub only by a valid one.

// MinPoolKeyBytes is the shortest pool key SetPoolKey accepts.
const MinPoolKeyBytes = 32

// MAC domains separate the uses of the key. Each is written as is, before the
// length-prefixed fields, so none may be a prefix of another (see
// TestMAC_DomainsAreDistinct).
const (
	poolMACDomain   = "firerunner/ec2-pool-mac/v1"
	diskMACDomain   = "firerunner/ec2-disk-mac/v1"
	runnerMACDomain = "firerunner/ec2-runner-mac/v1"
)

// SetPoolKey sets the key that signs and verifies pool instances and runner
// tags. Call it before any tier runs. Without it, the client signs with a
// random key of its own, so a restarted process trusts no pool instance and
// rebuilds the pool, and its reaper deregisters no runner an earlier process
// launched (MaxLifetime ends those instances).
func (c *Client) SetPoolKey(key []byte) error {
	if len(key) < MinPoolKeyBytes {
		return fmt.Errorf("pool key: %d bytes, want at least %d", len(key), MinPoolKeyBytes)
	}
	c.poolKey = append([]byte(nil), key...)
	c.poolKeyEphemeral = false
	return nil
}

// PoolKeyEphemeral reports whether the pool key is this process's own random
// one (no SetPoolKey), which no later process can verify.
func (c *Client) PoolKeyEphemeral() bool { return c.poolKeyEphemeral }

func randomPoolKey() []byte {
	k := make([]byte, MinPoolKeyBytes)
	if _, err := rand.Read(k); err != nil {
		// crypto/rand does not fail on a supported platform; a fixed key
		// would trust forged instances, so never go on without one.
		panic(fmt.Sprintf("pool key: %v", err))
	}
	return k
}

// mac is the hex HMAC-SHA256, under the pool key, of a domain string and the
// fields, each length-prefixed so that no two different lists encode alike.
func (c *Client) mac(domain string, fields ...string) string {
	m := hmac.New(sha256.New, c.poolKey)
	m.Write([]byte(domain))
	for _, s := range fields {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		m.Write(n[:])
		m.Write([]byte(s))
	}
	return hex.EncodeToString(m.Sum(nil))
}

// checkMAC compares a tag's value with the MAC it should hold. The
// comparison takes the same time wherever the tag differs.
func checkMAC(got, want string, missing, bad error) error {
	if got == "" {
		return missing
	}
	if !hmac.Equal([]byte(got), []byte(want)) {
		return bad
	}
	return nil
}

// poolMAC signs a pool instance's owner, tier and instance ID.
func (c *Client) poolMAC(owner, tier, id string) string {
	return c.mac(poolMACDomain, owner, tier, id)
}

var (
	errNoPoolMAC  = errors.New("no " + TagPoolMAC + " tag")
	errBadPoolMAC = errors.New(TagPoolMAC + " does not match this install's pool key, owner, tier and instance")
)

// verifyPoolMAC checks an instance's TagPoolMAC against the owner and tier
// the caller expects of it and its own ID.
func (c *Client) verifyPoolMAC(i types.Instance, owner, tier string) error {
	want := c.poolMAC(owner, tier, aws.ToString(i.InstanceId))
	return checkMAC(tagValue(i.Tags, TagPoolMAC), want, errNoPoolMAC, errBadPoolMAC)
}

// diskOf is what TagPoolDiskMAC signs of an instance as EC2 describes it: its
// image, its root device name and every block device mapping, as
// "device=volume@attach-time/delete-on-termination" sorted. The attach time
// tells a volume detached and attached again (to be altered elsewhere) from
// the one attached at launch: same ID, later time. ok is false while EC2
// lists no EBS volume on the root device yet, as it may not right after
// RunInstances.
func diskOf(i types.Instance) (image, root string, volumes []string, ok bool) {
	image, root = aws.ToString(i.ImageId), aws.ToString(i.RootDeviceName)
	for _, m := range i.BlockDeviceMappings {
		dev, vol := aws.ToString(m.DeviceName), ""
		if e := m.Ebs; e != nil {
			vol = fmt.Sprintf("%s@%s/%t", aws.ToString(e.VolumeId), aws.ToTime(e.AttachTime).UTC().Format(time.RFC3339Nano), aws.ToBool(e.DeleteOnTermination))
		}
		volumes = append(volumes, dev+"="+vol)
		ok = ok || (dev == root && m.Ebs != nil && aws.ToString(m.Ebs.VolumeId) != "")
	}
	slices.Sort(volumes)
	return image, root, volumes, ok
}

// diskMAC signs a pool instance's image, root device and volumes, with its
// owner, tier and ID.
func (c *Client) diskMAC(owner, tier, id, image, root string, volumes []string) string {
	return c.mac(diskMACDomain, append([]string{owner, tier, id, image, root}, volumes...)...)
}

var (
	errNoDiskMAC  = errors.New("no " + TagPoolDiskMAC + " tag")
	errBadDiskMAC = errors.New(TagPoolDiskMAC + " does not match the instance's image and volumes")
)

// verifyDiskMAC checks, by its TagPoolDiskMAC, that an instance's image and
// volumes are still the ones it was launched with. The error says what it has
// now, for the log.
func (c *Client) verifyDiskMAC(i types.Instance, owner, tier string) error {
	image, root, volumes, _ := diskOf(i)
	want := c.diskMAC(owner, tier, aws.ToString(i.InstanceId), image, root, volumes)
	if err := checkMAC(tagValue(i.Tags, TagPoolDiskMAC), want, errNoDiskMAC, errBadDiskMAC); err != nil {
		return fmt.Errorf("%w (now image %s, root %s, volumes %s)", err, image, root, strings.Join(volumes, " "))
	}
	return nil
}

// runnerMAC signs the runner name on a job instance, with its owner, tier and
// ID: the reaper deregisters that runner in that tier.
func (c *Client) runnerMAC(owner, tier, runner, id string) string {
	return c.mac(runnerMACDomain, owner, tier, runner, id)
}

var (
	errNoRunnerMAC  = errors.New("no " + TagRunnerMAC + " tag")
	errBadRunnerMAC = errors.New(TagRunnerMAC + " does not match this install's pool key, owner, tier, runner and instance")
)

// verifyRunnerMAC checks an instance's TagRunnerMAC against the owner, tier
// and runner the caller read from its tags, and its own ID.
func (c *Client) verifyRunnerMAC(i types.Instance, owner, tier, runner string) error {
	want := c.runnerMAC(owner, tier, runner, aws.ToString(i.InstanceId))
	return checkMAC(tagValue(i.Tags, TagRunnerMAC), want, errNoRunnerMAC, errBadRunnerMAC)
}
