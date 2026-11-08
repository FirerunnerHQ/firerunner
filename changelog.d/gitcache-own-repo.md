### Fixed

- `git_cache` saves only the job's own repository. A fetch of another one, such
  as a second checkout or a submodule, was stored under the job's repository
  and default branch, where a fork PR could restore it. Git cache keys now
  name their repository; entries saved before this version are never restored,
  to the proxy or any other client. Images built before this version keep
  fetching from GitHub but no longer save to the git cache; rebuild them to
  get it back.
