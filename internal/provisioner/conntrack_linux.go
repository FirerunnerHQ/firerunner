//go:build linux

package provisioner

import (
	"errors"
	"fmt"
	"net/netip"
	"syscall"

	"github.com/mdlayher/netlink"
	"github.com/ti-mo/conntrack"
)

// flushGuestConntrack deletes every conntrack entry in the daemon's network
// namespace that one of guestIPs originated.
func flushGuestConntrack(guestIPs ...string) error { return flushConntrackIn(0, guestIPs...) }

// flushConntrackIn deletes the conntrack entries whose original source is one
// of guestIPs, in the network namespace open as netnsFD (0 for the current
// one). The kernel filters a flush only by mark or zone, so this lists the
// table once and deletes the guests' entries one by one; the connection limits
// keep the table, and so the listing, bounded (see conntrackEntries). An entry
// that is gone by the time it is deleted is not an error.
func flushConntrackIn(netnsFD int, guestIPs ...string) error {
	guests := make(map[netip.Addr]bool, len(guestIPs))
	for _, s := range guestIPs {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("guest address %q: %w", s, err)
		}
		guests[ip] = true
	}
	if len(guests) == 0 {
		return nil
	}
	c, err := conntrack.Dial(&netlink.Config{NetNS: netnsFD})
	if err != nil {
		return fmt.Errorf("open conntrack: %w", err)
	}
	defer c.Close()
	flows, err := c.Dump(nil)
	if err != nil {
		return fmt.Errorf("list conntrack: %w", err)
	}
	var errs []error
	for _, flow := range flows {
		if !guests[flow.TupleOrig.IP.SourceAddress] {
			continue
		}
		if err := c.Delete(flow); err != nil && !errors.Is(err, syscall.ENOENT) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("delete %d conntrack entries: %w", len(errs), errors.Join(errs...))
	}
	return nil
}
