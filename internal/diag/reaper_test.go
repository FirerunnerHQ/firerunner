package diag

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
)

// status lists what the reaper left alone for want of a MAC, and doctor warns
// while there is any, saying how to tell whose it is.
func TestStatusAndDoctorShowUnprovenInstances(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	f := health.File{PID: 1, WrittenAt: now,
		Tiers:  []health.Tier{{Name: "small", State: health.Connected, Since: now}},
		Reaper: &health.Reaper{At: now.Add(-time.Minute), Unproven: []string{"i-0aaa", "sir-0bbb"}}}
	write := func() {
		if err := health.Write(filepath.Join(dir, health.FileName), f); err != nil {
			t.Fatal(err)
		}
	}
	write()
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: dir, TapPrefix: "fr", NetBase: 16}}
	var text bytes.Buffer
	if err := Status(cfg, "test", &text, false); err != nil {
		t.Fatal(err)
	}
	if want := "2 left alone: this install's owner tag, but no MAC proves it launched them: i-0aaa sir-0bbb"; !strings.Contains(text.String(), want) {
		t.Errorf("status missing %q\n%s", want, text.String())
	}
	check := func() *Check {
		for _, c := range listenerChecks(cfg.Firecracker, now) {
			if c.Name == "ec2-reaper" {
				return &c
			}
		}
		return nil
	}
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "i-0aaa sir-0bbb") || !strings.Contains(c.Detail, "CloudTrail") {
		t.Fatalf("doctor with unproven instances = %+v", c)
	}
	f.Reaper.Unproven = nil
	write()
	if c := check(); c != nil {
		t.Fatalf("doctor with none unproven = %+v", c)
	}
}
