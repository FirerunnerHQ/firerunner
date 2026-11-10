package ec2

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// A pool instance's tags are all a restarted firerunner knows of it, and
// anyone in the account who may tag an instance can write them. So each pool
// instance carries TagPoolMAC: an HMAC of its owner, tier and instance ID
// under a key only the control plane holds, tagged as soon as RunInstances
// names the instance. The pool takes in an instance only with a valid MAC:
// one without is never registered for a JIT config nor started.

// MinPoolKeyBytes is the shortest pool key SetPoolKey accepts.
const MinPoolKeyBytes = 32

// poolMACDomain separates the pool MAC from any other use of the key.
const poolMACDomain = "firerunner/ec2-pool-mac/v1"

// SetPoolKey sets the key that signs and verifies pool instances. Call it
// before any tier runs. Without it, the client signs with a random key of its
// own, so a restarted process trusts no pool instance and rebuilds the pool.
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

// poolMAC is the hex HMAC-SHA256, under the pool key, of a domain string and
// the owner, tier and instance ID, each length-prefixed so that no two
// different triples encode alike.
func (c *Client) poolMAC(owner, tier, id string) string {
	m := hmac.New(sha256.New, c.poolKey)
	m.Write([]byte(poolMACDomain))
	for _, s := range []string{owner, tier, id} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		m.Write(n[:])
		m.Write([]byte(s))
	}
	return hex.EncodeToString(m.Sum(nil))
}

var (
	errNoPoolMAC  = errors.New("no " + TagPoolMAC + " tag")
	errBadPoolMAC = errors.New(TagPoolMAC + " does not match this install's pool key, owner, tier and instance")
)

// verifyPoolMAC checks an instance's TagPoolMAC against the owner and tier
// the caller expects of it and its own ID. The comparison takes the same time
// wherever the tag differs.
func (c *Client) verifyPoolMAC(i types.Instance, owner, tier string) error {
	got := tagValue(i.Tags, TagPoolMAC)
	if got == "" {
		return errNoPoolMAC
	}
	want := c.poolMAC(owner, tier, aws.ToString(i.InstanceId))
	if !hmac.Equal([]byte(got), []byte(want)) {
		return errBadPoolMAC
	}
	return nil
}
