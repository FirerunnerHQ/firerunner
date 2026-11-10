### Security

- On EC2, the cache identity finds a runner's job by the runner's GitHub ID,
  from its JIT registration, as well as its name, which a runner registered in
  another scope could share. A runner whose ID is unknown is not looked up;
  JobStarted resolves it instead.
