# Changelog

## Unreleased

### Fixed

- `CheckAuth` requires a non-empty `userid` in the `user.checkAuthentication`
  response: a well-formed envelope carrying an empty object no longer passes
  for a verified token.
- Time-period conditions cap the range end at `24:00` (a `24:59` typo now
  fails the plan, not the apply), and interface `dns` rejects characters
  outside `A-Za-z0-9._-` at plan time.
- Non-numeric values coming back in an action refuse with the same
  `terraform state rm` hint as every other unmanageable shape.
- Deleting a host group that still contains hosts explains why Zabbix refused
  instead of surfacing a bare application error.
- An agent interface deleted externally between plan and apply reports the
  same clear "vanished" error as the other resources.
- Host groups created by a discovery rule's group prototype (`flags=4`) are
  refused like discovered hosts: Read refuses to adopt them and Update/Delete
  are fail-closed (a response without a `flags` field refuses the mutation).
- Reads are fail-closed on the full schema ranges: an out-of-range media type
  value (enum, port, attempt counters, intervals) or an action shape outside
  the supported model (unknown `evaltype`/`default_msg`, invalid escalation
  steps or condition values) is refused instead of adopted into state; the
  resolved-value validation before mutations repeats every schema range.
- Binary API fields (host/media type/action `status`, `useip`, SMTP verify
  flags, webhook menu flags, action pause flags) and interface addresses are
  refused when unrepresentable instead of being normalised to false/defaults;
  the docs describe the explicit-over-environment credential precedence.
- Partial API responses are refused too: a MISSING required binary bit
  (status, pause or verify flags) or an incomplete host interface entry
  (no `interfaceid`/`type`/`main`) cannot masquerade as false or as "no
  agent interface".
- The restricted-read probe no longer treats `description` as proof of a
  full `mediatype.get` response (Zabbix 7.0 includes it in the restricted
  set): a Script/SMS read under a non-Super-Admin role refuses clearly
  instead of faking drift. Release tags `v*` are protected by a repository
  ruleset; the acceptance token cleanup surfaces failures.
- Email rules now match the verified API behaviour on 6.4 and 7.0:
  `smtp_helo` is no longer required (Zabbix derives HELO from the sender
  domain), and `smtp_verify_peer`/`smtp_verify_host` fail the plan when
  `smtp_security` is 0 (the API rejects that combination in apply).
- A transport failure during the read confirming an "object missing" delete
  surfaces both causes; `localhost` is recognised case-insensitively; the
  acceptance helper client honours `ZABBIX_TLS_INSECURE`/`ZABBIX_CA_CERT_FILE`.

### Changed

- Dependabot also watches the digest-pinned acceptance images (docker-compose
  ecosystem), so the tested Zabbix/Postgres versions do not silently age.
- The action acceptance test verifies that its helper resources (media type,
  host groups) are destroyed too.
- New acceptance test imports an object created outside Terraform (raw API
  seed) and asserts an empty plan; the README documents the unauthenticated
  `apiinfo.version` probe and the stock objects the acceptance suite expects.
- The CI acceptance matrix asserts the Zabbix version it actually talks to,
  and the partial-state regression test covers hosts as well.
- The acceptance matrix also runs against Zabbix 6.4.1 - the declared minimum
  version - via a digest-pinned overlay.

## v0.2.2 (2026-08-31)

### Added

- `zabbix_media_type.email_provider` - the Email SMTP provider preset (API
  field `provider`: generic SMTP, Gmail, Gmail relay, Office365, Office365
  relay); previously invisible to Terraform, so preset drift was undetectable.

### Fixed

- A webhook `timeout` outside 1-60s coming back from the API is refused at
  Read (fail-closed) instead of poisoning the state.
- The low-level-discovery barrier holds on every mutating path: Update and
  Delete refuse LLD-owned hosts even under `-refresh=false`, and they are
  fail-closed when the API response carries no `flags` field at all.
- A host group deleted externally between plan and apply reports the same
  clear "vanished" error as the other resources.
- The README documents the two bootstrap calls that carry a secret in the
  request body (`user.login`, `user.checkAuthentication`) and the CI badge
  points at the publishing repository.

## v0.2.1 (2026-08-31)

### Fixed

- An unknown `eventsource` is rejected at plan time: ForceNew with an unknown
  value would plan a destructive replace whose Create can reject the resolved
  value after the action was already deleted.
- A JSON-RPC error object missing the mandatory `code`/`message` fields is
  treated as malformed; a forged bare `data` carrying the session-expiry
  marker can no longer trigger a re-login and a retried mutation.
- Documentation and tooling uniformly reference the `SychPL/zabbix` registry
  namespace; CI blocks stale references.

## v0.2.0 (2026-08-31) - hardening

### Breaking changes

- The provider source is `SychPL/zabbix` (previously `adi/...`), matching the
  repository and registry namespace that publish the releases; the Go module
  path stays `github.com/Tensai123/terraform-provider-zabbix` (code-only,
  invisible to Terraform users).

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
  as a warning in the plan output. Requests are single-shot on transport
  errors (never replayed); the one exception is a session rejected as
  expired, which is renewed and the rejected request repeated exactly once
  (Zabbix refuses such requests before executing anything). The JSON-RPC
  envelope is validated before any error handling.
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
- `zabbix_action`: an empty `subject`/`message` with `default_msg = false` is
  now sent explicitly; the API merges omitted fields, which kept the stale
  value and produced a perpetual diff.
- Failed updates no longer write the planned values into the Terraform state
  (an SDKv2 default); partial mode keeps the previous state until the API
  confirms the mutation.
- A create whose immediate follow-up read returns empty keeps the new ID and
  fails loudly instead of orphaning the object outside the state.
- Mutation responses are verified against the mutated object's ID; empty or
  foreign IDs are errors.
- The version gate is fail-closed: an unparsable 6.4 patch level (e.g. a
  release candidate) gets the "untested" warning instead of silently passing.
- JSON-RPC envelope: a present `error` member (even `null`) next to `result`
  is treated as malformed instead of a success.
- Actions with recovery or update operations are refused at Read (previously
  they were silently left unmanaged after an import).
- A restricted `mediatype.get` response (non-Super-Admin roles since Zabbix
  6.4.19) is a hard error instead of silently zeroing the configuration.
- An explicit `password` next to an explicit `api_token` is a conflict even
  when `username` comes from the environment.
- Plan-time validation: action condition values must be non-empty (severity
  0-5), host `dns` is validated (user macros allowed); updating a host that
  was deleted externally reports a clear error.
- Cross-field rules are re-validated on resolved values at apply time (plan
  validation must skip unknown references): host address, action operations
  and media type per-type requirements fail instead of silently degrading.
- Updating an action refuses to overwrite recovery/update operations or
  operation conditions added outside Terraform since the last refresh.
- A create that fails with a transport error reports the unknown outcome and
  suggests an import check instead of inviting a duplicating retry.
- Release workflow: read-only ancestry preflight runs before CI and before
  the release-environment approval; write permission is limited to the
  signing job.
- Responses larger than 32 MiB fail with an explicit size error instead of a
  misleading JSON parse failure after silent truncation.
- Partial state mode starts before update validation and preflight reads, so
  an error before the first mutation cannot persist planned values either.
- Updating an action refuses every shape Read refuses (full mapping check);
  a media type's parameters are cleared based on the API-current type, so an
  external type drift between plan and apply cannot leave them behind.
- Early configure errors (missing credentials, TLS conflicts) still carry the
  plain-HTTP and TLS warnings.
- Equivalent spellings of the same duration (`3600` vs `1h`) no longer cause
  a perpetual diff on `esc_period`, `attempt_interval` and `timeout`.
- An action operation without an `opmessage` object is refused instead of
  being read as "default message to everyone".
- A `user.login` response with an empty session token is rejected; requests
  can no longer go out silently unauthenticated.
- The media type update preflight refuses exactly what Read refuses; an
  unrepresentable shape gained between plan and apply is no longer mutated.
- An unconfigured visible host name follows the resolved technical name even
  when the rename was unknown at plan time.
- Set elements (group, template, user and user-group IDs) and time-period
  condition values are validated at plan time.
- Removing the managed agent interface is idempotent: a parallel deletion
  between the preflight read and the delete no longer fails the apply.
- Webhook parameter names are validated at plan time; raw-config reads are
  guarded against partial objects.
- A missing `url` reports "url must be configured" instead of a misleading
  format error; `dns` length is counted in characters, not bytes; the
  standard Go proxy environment is documented in the README.
- A resolved `operationtype` other than "send message" fails before the
  create instead of stranding a freshly created action as unmanageable;
  the same applies to resolved `evaltype`, `conditiontype` and media type
  `type` values.
- The release build matrix excludes windows/arm (the target was removed in
  Go 1.25); cross-compilation no longer fails before producing artifacts.
- Import examples use environment-variable placeholders instead of hardcoded
  object IDs that could match real production objects.

### Added

- `zabbix_media_type`: full Zabbix 6.4 object model - `description`,
  `max_sessions`, `max_attempts`, `attempt_interval`, `content_type` (email),
  `process_tags`, `show_event_menu`, `event_menu_url`, `event_menu_name`
  (webhook). Changing them outside Terraform now shows up as drift.
- The provider rejects Zabbix 6.4.0 at configure time with a clear diagnostic
  (`user.checkAuthentication` cannot validate API tokens before 6.4.1).
- Configure warnings (plain HTTP, disabled TLS verification, ignored ambient
  credentials) are reported even when configure fails.
- `zabbix_action.pause_symptoms` (pause escalation for symptom problems,
  Zabbix 6.4).
- The CI acceptance matrix covers Zabbix 7.0 LTS next to 6.4; 7.0.x no
  longer triggers the "untested" warning.
- `api_token` authentication (Bearer header, validated at configure time with
  `user.checkAuthentication`, independent of the token's method permissions),
  `tls_insecure` (warns when active), `ca_cert_file` (added to the system
  trust store; mutually exclusive with `tls_insecure`, also when set through
  the environment). Every mutation verifies a typed response with the
  expected object IDs. HTTP redirects are never followed and
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
  unlinked. A host can be created without any interface (leave `ip` and `dns`
  empty, e.g. for trapper or dependent items); adding an address later creates
  the agent interface and removing it deletes the interface. Hosts created by
  low-level discovery are refused.
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
