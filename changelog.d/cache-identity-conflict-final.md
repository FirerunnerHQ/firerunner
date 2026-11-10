### Security

- A runner denied the cache because two sources named different jobs for it
  stays denied. A lookup still running for the first job could restore its
  access when it finished.
