# Terraform Provider for Zabbix

[![CI](https://github.com/Tensai123/terraform-provider-zabbix/actions/workflows/ci.yml/badge.svg)](https://github.com/Tensai123/terraform-provider-zabbix/actions/workflows/ci.yml)

A Terraform provider for managing Zabbix objects through the JSON-RPC API.
Tested against **Zabbix 6.4 and 7.0 LTS** (acceptance matrix in CI, see
[docker-compose.acc.yml](docker-compose.acc.yml)).
Zabbix 6.4.1 or newer is required: 6.4.0 cannot validate API tokens
(`user.checkAuthentication` gained its `token` parameter in 6.4.1) and is
rejected at configure time; other version lines produce an "untested" warning.

## Resources

| Resource | Description |
|---|---|
| `zabbix_host_group` | Host groups |
| `zabbix_host` | Hosts with an optional main agent interface (IP or DNS), groups, templates, visible name, status |
| `zabbix_media_type` | Email (incl. SMTP auth/TLS), script, SMS and webhook media types, incl. retry/concurrency options and the webhook event menu |
| `zabbix_action` | Trigger actions with conditions and "send message" operations |

All resources support `terraform import` using the Zabbix object ID.
Generated reference documentation lives in [docs/](docs/).

## Usage

```hcl
terraform {
  required_providers {
    zabbix = {
      source = "Tensai123/zabbix"
    }
  }
}

provider "zabbix" {
  url       = "https://zabbix.example.com/api_jsonrpc.php"
  api_token = var.zabbix_api_token # or username + password
}

resource "zabbix_host_group" "servers" {
  name = "Linux servers"
}

resource "zabbix_host" "web01" {
  host      = "web01"
  groups    = [zabbix_host_group.servers.id]
  # Template IDs differ between installations: look yours up with
  # template.get (or Data collection -> Templates in the UI).
  templates = [var.linux_template_id]
  ip        = "192.0.2.10"
}
```

### Provider configuration

| Argument | Env variable | Description |
|---|---|---|
| `url` | `ZABBIX_URL` | API endpoint, e.g. `https://zabbix.example.com/api_jsonrpc.php` |
| `api_token` | `ZABBIX_API_TOKEN` | API token (Administration -> API tokens). Recommended. |
| `username` / `password` | `ZABBIX_USERNAME` / `ZABBIX_PASSWORD` | Alternative to `api_token`. The session is renewed automatically when it expires. |
| `tls_insecure` | `ZABBIX_TLS_INSECURE` | Skip TLS verification (testing only) |
| `ca_cert_file` | `ZABBIX_CA_CERT_FILE` | PEM bundle used to verify the server certificate |

A warning is emitted when a non-loopback `http://` URL is used: credentials and
tokens are then sent in clear text.

### Sensitive values

`password`, `api_token`, media type `password` and webhook `parameter.value`
are marked sensitive: they are masked in CLI output but still stored in the
Terraform state. Use an encrypted, access-controlled state backend.

## Behaviour worth knowing

- **Read errors never drop resources from state.** Only a confirmed "not found"
  (empty API result) removes a resource; transport errors and timeouts are
  surfaced as errors. An expired session is renewed once for username/password
  authentication and the rejected request repeated (safe: Zabbix refuses such
  requests before executing anything); the error surfaces only when the
  re-login or the repeated request fails. Note that Zabbix returns an empty result also
  when the user has no permission to see the object. The same applies to
  `terraform destroy`: an object the credentials can no longer see is treated
  as already deleted (the Zabbix API cannot distinguish the two cases), so use
  credentials whose permissions cover everything under management.
- **`zabbix_host` only manages the main agent interface** - and the interface
  is optional: leave `ip` and `dns` empty to create the host without one (for
  trapper or dependent items). The interface is updated with
  `hostinterface.update` and created/deleted when the address appears in or
  disappears from the configuration; other interfaces (SNMP, IPMI, JMX) are
  never touched.
  Templates removed from `templates` are unlinked with `templates_clear`, which
  also removes their inherited items and triggers.
- **`zabbix_media_type` owns all attributes of the object.** Attributes that do
  not belong to the configured `type` are rejected at plan time, and changing
  `type` deliberately resets the previous type's attributes in Zabbix
  (including SMTP credentials and webhook parameters) so no stale secrets
  linger.
- **`zabbix_action`** supports trigger actions (`eventsource = 0`, ForceNew) with
  "send message" operations to user groups and/or users. `condition` is a set,
  so ordering does not produce diffs.
- Every CRUD operation defaults to a 2-minute timeout; raise it per resource
  with a `timeouts` block (e.g. `timeouts { create = "15m" }`) when an apply
  links large templates.
- Standard Go proxy variables (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`) are
  honoured: API traffic - including credentials and tokens - then flows
  through the configured proxy. Unset them (or use `NO_PROXY`) when the
  Zabbix API must be reached directly.
- The provider authenticates every request with an `Authorization: Bearer`
  header only. If configure succeeds but mutations fail with "Not authorized",
  a proxy in front of Zabbix is probably stripping the header (compare
  [ZBX-22952](https://support.zabbix.com/browse/ZBX-22952)).
- Deletes are idempotent: an object already removed in Zabbix does not fail
  `terraform destroy`.
- **Objects the provider cannot represent are refused, not rewritten.** If an
  imported or externally modified object uses features outside the supported
  model (action operation types other than "send message", operation
  conditions, custom condition expressions, non-trigger event sources, script
  media type parameters, media type types other than email/script/SMS/webhook,
  hosts created by low-level discovery, restricted media type reads),
  `Read` fails with an explanation. Because refresh runs before `plan` and
  `destroy`, detach such an object with `terraform state rm <address>` (or
  change it in Zabbix) instead of letting Terraform overwrite it.

## Development

Requirements: Go (see `go.mod`), Terraform >= 1.0, Docker.

```sh
go build ./...
go test ./...                       # unit tests (no network)
```

### Acceptance tests

```sh
docker compose -f docker-compose.acc.yml up -d --wait   # Zabbix 6.4 on http://localhost:8082 (Admin / zabbix)
# Wait for the server's first-start DB maintenance (API writes block on its
# locks) and give the fresh PostgreSQL planner statistics - without these two
# steps host tests can stall on locks for minutes:
until docker compose -f docker-compose.acc.yml logs zabbix-server | grep -q 'server #0 started'; do sleep 5; done
docker compose -f docker-compose.acc.yml exec -T zabbix-db psql -U zabbix -q -c "ANALYZE"
TF_ACC=1 ZABBIX_URL=http://localhost:8082/api_jsonrpc.php \
  ZABBIX_USERNAME=Admin ZABBIX_PASSWORD=zabbix \
  go test ./zabbix -run TestAcc -count=1 -v
docker compose -f docker-compose.acc.yml down -v
```

Append `-f docker-compose.acc-70.yml` to every `docker compose` call above to
run the same suite against Zabbix 7.0 LTS.

### Local provider override

Build the binary and point Terraform at the checkout with a
[development override](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides):

```hcl
# ~/.terraformrc (Linux/macOS) or %APPDATA%\terraform.rc (Windows)
provider_installation {
  dev_overrides {
    "Tensai123/zabbix" = "/path/to/terraform-provider-zabbix"
  }
  direct {}
}
```

```sh
go build -o terraform-provider-zabbix .        # Linux/macOS
go build -o terraform-provider-zabbix.exe .    # Windows (dev_overrides needs the .exe suffix)
cd example_deployment && terraform apply
```

### Documentation

`docs/` is generated with [tfplugindocs](https://github.com/hashicorp/terraform-plugin-docs)
from the schema descriptions and `examples/`:

```sh
go generate ./...
```

CI fails when `docs/` is out of date.

## Release

Tagging `v*` runs GoReleaser (see `.github/workflows/release.yml`), which builds,
signs and publishes the binaries for the Terraform Registry.

One-time repository setup the workflow relies on:

- create the `release` environment and add **required reviewers** - the job
  only pauses for approval (protecting the signing key) when the environment
  is actually protected in the repository settings;
- store `GPG_PRIVATE_KEY` / `GPG_PASSPHRASE` as **environment secrets** of
  `release` (not repository secrets), so no other workflow can read them;
- the workflow itself refuses tags that are not reachable from `main`.

## License

See [LICENSE](LICENSE).
