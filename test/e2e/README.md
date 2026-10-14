# firerunner end-to-end tests

These tests boot **real Firecracker microVMs** on Linux with `/dev/kvm`.
They require Firecracker on `PATH`, `ip`, `nft`, and root/sudo privileges.
The rootfs builder also requires Go, `mkfs.ext4`, loop mounts and `mknod`.

## Run boot and cache isolation tests

From the repository root:

```bash
# Rebuild after changing smoke_init.go. This is a dedicated disposable image.
sudo ./test/e2e/build-smoke-rootfs.sh /var/tmp/fr-smoke/rootfs.ext4
FR_KERNEL=/absolute/path/to/vmlinux \
FR_GOLDEN=/var/tmp/fr-smoke/rootfs.ext4 make e2e
```

`run.sh` compiles the test binary as the caller, then uses root/sudo to create a
throwaway network namespace with a dummy external interface. All taps, routes
and nftables rules stay in that namespace, which is deleted on exit. No external
network access, GitHub credentials or existing runner services are needed.
Missing prerequisites fail this command rather than silently skipping tests.
The guest kernel needs virtio networking, static kernel IP configuration and ext4.

The smoke rootfs contains a static Go PID1 at `/sbin/init`. Each guest must print
an explicit success marker, reboot and exit before the deadline. Cancellation
alone is not success. Tests assert that the tap and network slot are reclaimed.

`TestCacheScopesAcrossMicroVMs` keeps one real cache server and job registry
across ten sequential VMs, reusing the same guest IP:

| Guest | Checks |
| --- | --- |
| main | Upload/finalize shared and default-only entries |
| base branch | Restore default, then save its own shared entry |
| PR | Base/default fallback, own-ref writes and precedence, forged metadata ignored |
| sibling PR | Cannot read the first PR's entries, including empty-prefix search |
| same PR rerun | Can restore its own prior entry |
| main after PR | Cannot read PR-only entries; shared entry remains MAIN |
| unknown identity | Cannot read or write any repository |
| repository B | Cannot read A; saves/restores its own identical key |
| repository A again | Still reads its original entry after B writes |
| resolved read-only identity | Default restore works; writes denied |

Every cache operation originates in the guest and traverses the real tap,
source-IP firewall binding, HTTP server, identity reader and cache storage.
Every VM exit must revoke its job record before the IP is reused.
Only the GitHub resolver is a host fixture. The separate `test/cache-protocol`
suite exercises real `@actions/cache` archive upload/extraction; unit and netns
tests cover stale connections, delayed requests, spoofing and failure paths.

To invoke the compiled test binary directly, set `FR_KERNEL`, `FR_GOLDEN`, and
`FR_EXT_IFACE` inside a disposable network namespace. Set `FR_REQUIRE_E2E=1`
to fail on missing prerequisites; without it, direct Go test invocations skip.
The `e2e` build tag is required.

## Full GitHub job test

A real GitHub Actions assignment additionally needs a firerunner-compatible
golden image (`actions/runner` plus MMDS-JIT boot service; see
[`../../images/`](../../images/README.md)) and a GitHub App or PAT pointed at a
test repository. Exercise that through the `firerunner` binary itself.
The smoke/cache suite does not validate live GitHub assignment or API resolution.
