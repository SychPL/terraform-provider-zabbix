# Changelog

## Unreleased (v0.2.0) - hardening

### Breaking changes

- Module path and provider source are now `github.com/Tensai123/terraform-provider-zabbix`
  / `Tensai123/zabbix` (previously `adi/...`), matching the repository that
  publishes releases.

- `zabbix_host.ip` is now optional; `use_ip`/`dns` select DNS-based connections.
- `zabbix_action.condition` is a set (was a list): remove and re-add the
  resource from state if you rely on ordering (the provider was not published
  before this change).
- `zabbix_action.operation.mediatypeid` defaults to `"0"` (all media types)
  instead of `""`.
- `zabbix_media_type`: attributes not relevant for the selected `type` are
  rejected at plan time (e.g. `parameter` blocks on an email media type).

### Fixed

- Read no longer removes a resource from state on transport errors, timeouts
  or expired sessions; only a confirmed "not found" does, and that is reported
  as a warning in the plan output. Requests are strictly single-shot (no
  transparent retries) and the JSON-RPC envelope is validated before any
  error handling.
- `zabbix_host` updates no longer replace the whole interface collection
  (which deleted SNMP/IPMI/JMX interfaces); the main agent interface is updated
  with `hostinterface.update`, and recreated with `hostinterface.create` when
  it was removed outside Terraform (the drift is visible in the plan).
- `zabbix_host` reads the correct interface (`type = 1`, `main = 1`) instead of
  the first one returned by the API.
- Unlinking templates uses `templates_clear` so inherited entities are removed.
- `zabbix_media_type`: renaming a webhook without parameters failed with
  `parameters: an array is expected`; parameters were never refreshed from the
  API (`selectParameters` is not a valid option).
- `zabbix_action`: `eventsource` is immutable (ForceNew) and no longer sent on
  update; recipients removed from configuration are now removed in Zabbix.
- Removed debug output of full API payloads to stderr.

### Added

- `api_token` authentication (Bearer header, validated at provider configure),
  `tls_insecure`, `ca_cert_file` (added to the system trust store; mutually
  exclusive with `tls_insecure`, also when set through the environment). HTTP redirects are never followed and
  malformed JSON-RPC responses are never treated as success; URLs with embedded
  credentials are rejected.
- Automatic single re-login when a username/password session expires; all
  concurrent callers share the result (including a failure), the login runs
  in the background with its own deadline (every caller still honours its
  own context) and a failed login is not retried for 30 seconds.
- `terraform import` for all resources. Objects the provider cannot represent
  (unsupported media type types, script parameters, action operation types,
  operation conditions, custom expressions) are refused with a hint instead of
  being silently rewritten.
- `timeouts` blocks and context propagation to HTTP requests; the HTTP client
  itself imposes no timeout, so `timeouts { create = "15m" }` is honoured.
- Idempotent deletes.
- `zabbix_host`: `name` (follows `host` unless configured; drift of a
  non-configured name shows in the plan), `enabled`, `description`, `use_ip`,
  `dns`. Templates linked outside Terraform are cleared on update, not only
  unlinked.
- `zabbix_media_type`: SMTP port/security/verification/authentication,
  `username`, `password`; sensitive webhook parameter values; plan-time
  validation per media type (attributes of other types are rejected, a type
  change clears the previous type's attributes in Zabbix). Script media types
  with command-line parameters are refused (not modelled) instead of having
  their parameters wiped.
- `zabbix_action`: `users` recipients, `condition.value2` (event tag value
  conditions), `pause_suppressed`, `notify_if_canceled`, validation of
  escalation steps, periods (60s-1w for the action, 0 allowed per operation)
  and recipients; at least one `operation` is required, as in Zabbix. The
  condition type/operator matrix is validated at plan time against the values
  Zabbix 6.4 actually accepts (verified empirically). Actions containing operation types,
  event sources, custom expressions or operation conditions the provider does
  not support are refused instead of silently rewritten (the error explains
  how to detach the resource with `terraform state rm`). Plan-time validation defers values that are
  not known until apply.
- Unit tests (mock JSON-RPC server) and acceptance tests against Zabbix 6.4.
- CI workflow (fmt, vet, race tests, generated docs check, acceptance tests
  on `docker-compose.acc.yml`, govulncheck, `terraform fmt` on examples,
  GoReleaser config check and snapshot build, `terraform validate` of all
  examples against the built provider), pinned GitHub Actions and
  GoReleaser; the release workflow runs the full CI gate first, requires the
  tag to be reachable from `main` and uses a `release` environment. CI also
  fails on any stdout/stderr/log writes in provider code. Acceptance images
  are pinned by digest.
- Dependencies updated (terraform-plugin-sdk v2.40, grpc, x/net, x/text) and
  Go 1.25.13; `govulncheck` reports no known vulnerabilities.
- Generated documentation in `docs/` and examples in `examples/`.
