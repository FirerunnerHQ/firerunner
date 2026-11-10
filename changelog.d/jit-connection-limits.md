### Security

- The EC2 JIT endpoint now limits how many connections one source address,
  and all of them together, can hold open, closes each connection after one
  request, and uses shorter read and write deadlines, so one runner cannot
  crowd out other runners fetching their configs.
