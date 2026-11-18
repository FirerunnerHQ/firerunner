### Fixed

- `onboarding.yaml`: the webhook ingress queued no delivery. API Gateway could not assume its role, whose trust policy named a source account that API Gateway does not give for an integration. Every delivery also got a 500 that GitHub showed as a failure, because the binary media type mangled the response; the body now goes base64 through the mapping template, and arrives byte-identical (measured with a non-ASCII body).
