# Changelog

## Unreleased (v0.2.0) - hardening

### Breaking changes

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
  or expired sessions; only a confirmed "not found" does.
- `zabbix_host` updates no longer replace the whole interface collection
  (which deleted SNMP/IPMI/JMX interfaces); the main agent interface is updated
  with `hostinterface.update`.
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

- `api_token` authentication (Bearer header), `tls_insecure`, `ca_cert_file`.
- Automatic single re-login when a username/password session expires.
- `terraform import` for all resources.
- `timeouts` blocks and context propagation to HTTP requests.
- Idempotent deletes.
- `zabbix_host`: `name`, `enabled`, `description`, `use_ip`, `dns`.
- `zabbix_media_type`: SMTP port/security/verification/authentication,
  `username`, `password`; sensitive webhook parameter values; plan-time
  validation per media type.
- `zabbix_action`: `users` recipients, `condition.value2` (event tag value
  conditions), `pause_suppressed`, `notify_if_canceled`, validation of
  escalation steps, periods and recipients. Actions containing operation types
  the provider does not support are refused instead of silently rewritten.
- Unit tests (mock JSON-RPC server) and acceptance tests against Zabbix 6.4.
- CI workflow (fmt, vet, race tests, generated docs check, acceptance tests
  on `docker-compose.acc.yml`), pinned GitHub Actions and GoReleaser; the
  release workflow runs the full CI gate first.
- Generated documentation in `docs/` and examples in `examples/`.
