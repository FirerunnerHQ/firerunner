### Fixed

- On EC2 runners the instance's cache proxy waited at most 30 s for a cache
  RPC, so a first restore waiting on the job's identity (up to 60 s) became a
  miss after 30 s. It now waits 90 s. Takes effect with a rebuilt runner AMI.
