# Contributing

Contributions are welcome. This project reverse-engineers the NVIDIA Playground API
for GLM-5.2; keep that context in mind when opening issues or pull requests.

- **Issues:** Use issues for bugs, captcha/Upstream-API changes, or feature requests.
  Include the upstream API response (status code + body) and the serve command you ran.
- **Pull requests:** Keep changes focused. New CLI tools belong under `cmd/`, library
  code under the package root or `internal/`. Run `go build ./...` and `go vet ./...`
  before submitting.
- **Upstream drift:** NVIDIA may change endpoints, headers, or the captcha flow.
  If the API breaks, open an issue with the captured request/response (redact tokens).
