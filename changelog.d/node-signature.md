### Security

- The image builders believe a Node.js release's `SHASUMS256.txt` only when its
  signature checks out against a Node.js release key kept in
  `images/keys/`, and print which key signed it. Before, a checksum file
  from the same server as the tarball could not catch a compromised server.
  Building Node unpinned now needs `gpg` and `gpgv` on the build host;
  `images/keys/update-nodejs-keys.sh` refreshes the keys when a new releaser
  joins.
