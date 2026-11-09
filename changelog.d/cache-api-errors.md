### Changed

- The cache server's errors follow the protocols it speaks. Cache RPCs answer
  each Twirp error code at the HTTP status the Twirp specification gives it (an
  oversized body is now `400 invalid_argument`, not 413), and an unknown
  service or method, a non-POST request or a body not sent as JSON is a
  `404 bad_route` in JSON, where Go's plain-text 404 or 405 used to make the
  toolkit retry. Blob uploads and downloads fail as Azure Storage does, with
  the code in `x-ms-error-code` and an XML `Error` body, including
  `416 InvalidRange` and `405 UnsupportedHttpVerb`; a full cache and a store
  outage stay `507`, which clients do not retry. Errors no longer carry host
  paths, and the server's own log names the entry.
