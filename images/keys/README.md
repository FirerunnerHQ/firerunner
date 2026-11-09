# Release keys

The keys the image builders check signed downloads against. A download whose
signature does not check out stops the build before it is used.

## Node.js

`nodejs-release-keys.asc` holds the Node.js release keys. Every Node release
signs its `SHASUMS256.txt` with one of them, and `lib/verify.sh` believes the
checksums in it only once `gpgv` accepts that signature. Expired keys stay,
because older releases are signed with them.

`nodejs-release-keys.list` names each key and the
[nodejs/release-keys](https://github.com/nodejs/release-keys) commit they came
from. Both files are written by `update-nodejs-keys.sh`, never by hand. It
fetches that repository at a commit, checks every key file against its
fingerprint, and refuses unless the keys match the release keys listed in
[nodejs/node's README](https://github.com/nodejs/node#release-keys), a second
source.

When a build fails with "is not signed by a Node.js release key", first
check whether a new releaser has joined, then:

```sh
images/keys/update-nodejs-keys.sh          # or: ... COMMIT, to pin one
git diff images/keys/nodejs-release-keys.list
```

Review the `.list` diff before you commit it. A new key there should match a
new releaser announced in nodejs/node.
