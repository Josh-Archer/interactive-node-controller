# Contributing

Use focused pull requests. Run `make verify` before opening one; changes to
the controller, chart, CRD, or release workflow must include the corresponding
tests or contract checks. Do not add direct consumer-cluster deployment logic
to this repository.

Report security issues privately as described in [SECURITY.md](SECURITY.md).
Releases follow [VERSIONING.md](VERSIONING.md).
