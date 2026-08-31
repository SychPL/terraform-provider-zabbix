---
name: zabbix-provider-contrib
description: Guide for contributing to the Zabbix Terraform Provider - architecture, hard rules established in v0.2, testing and release workflow.
---

# Zabbix Terraform Provider - Contribution Guide (v0.2)

Authoritative references: `SPEC.md` (requirements, verified API facts, review
decisions) and `README.md` (user-facing behaviour). Read both before changing
code - most "obvious simplifications" here were rejected for a documented
reason recorded in SPEC section 4a.

## Prerequisites

- Go (version pinned in `go.mod`), Terraform >= 1.0, Docker.
- Target lines: Zabbix 6.4 (baseline) and 7.0 LTS - both run in the CI
  acceptance matrix. Registry namespace: `SychPL/zabbix` (CI gates stale
  references); the Go module path stays `github.com/Tensai123/...`.

## Architecture

- `zabbix/client.go` - JSON-RPC client: Bearer auth, single-flight re-login
  with failure memo, strict envelope validation, single-shot requests
  (`GetBody = nil`, no redirects), mutation results verified against IDs.
- `zabbix/helpers.go` - shared validators, duration parsing/suppression,
  readError/deleteError/createError/readAfterCreate.
- `zabbix/provider.go` - provider schema, credential resolution via raw
  config, version gate (6.4.1 minimum, untested-line warning).
- `zabbix/resource_*.go` - one file per resource.

## Hard rules (do not regress)

1. Refuse, never guess: any API shape the provider cannot round-trip is
   rejected at Read AND in the update preflight with the `terraform state rm`
   hint (same mapping for both).
2. `d.Partial(true)` is the FIRST statement of every Update; exactly one call.
3. Every plan-time cross-check skipped for unknown references is repeated on
   resolved values in Create/Update before any mutation.
4. Reads are tolerant of foreign-type fields but fail-closed on
   unrepresentable own-type values and on restricted API responses.
5. No payload or secret logging (CI greps for it); HTTP error bodies never
   reach diagnostics.
6. Tests assert exact values, not shapes; deletions in edit scripts must
   assert counts (guarded replace silently skips removals).

## Testing

```sh
gofmt -l . && go vet ./... && go test -count=1 ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...
docker compose -f docker-compose.acc.yml up -d --wait   # + ANALYZE and server-start wait, see README
TF_ACC=1 ZABBIX_URL=http://localhost:8082/api_jsonrpc.php ZABBIX_USERNAME=Admin ZABBIX_PASSWORD=zabbix \
  go test ./zabbix -run TestAcc -count=1 -v
# Zabbix 7.0: append -f docker-compose.acc-70.yml to the compose commands.
go generate ./...   # docs; CI fails on drift
```

## Release

Tag `vX.Y.Z` on `main` and push - the pipeline (ancestry preflight, full CI
matrix, GoReleaser with GPG signing, release notes from CHANGELOG.md) does the
rest; the Terraform Registry ingests new versions automatically. Update the
CHANGELOG section for the tag first - the release fails without it.
