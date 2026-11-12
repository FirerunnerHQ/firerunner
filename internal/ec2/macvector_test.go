package ec2

import (
	"strings"
	"testing"
	"time"
)

// The reaper Lambda (deploy/cloudformation/reaper.yaml) checks these MACs in
// Python; deploy/cloudformation/test-reaper.py asserts the same vectors, so
// the two cannot drift apart unnoticed.
func TestMAC_VectorsSharedWithTheReaperLambda(t *testing.T) {
	c := NewClient(newFakeEC2(t), nil, time.Hour, quietLog())
	if err := c.SetPoolKey([]byte(strings.Repeat("k", 32))); err != nil {
		t.Fatal(err)
	}
	for got, want := range map[string]string{
		c.poolMAC("acme", "small", "i-0aaa"):                  "afac3042f858ac4991846661b231d8737442fe01cfa237ffddcb448f5062a514",
		c.runnerMAC("acme/t1", "small", "runner-1", "i-0bbb"): "0998b25ee7b05b1e1ec6f07bdb111d569bebfa0bb3697a20b101c5307ceb4542",
		c.launchMAC("acme", "small", "token-0ddd"):            "dca78794a1f22b0ce988b1ace43b7b061d269824be44abc8b3ad577af18af2a1",
		c.requestMAC("acme", "small", "sir-0ccc"):             "868a83491aee3e25f37ecc2615cc691260a4af6167018b4f3bf0928ad955ccc4",
	} {
		if got != want {
			t.Errorf("MAC %s, want %s", got, want)
		}
	}
}
