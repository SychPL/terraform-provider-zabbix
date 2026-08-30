---
name: zabbix-provider-contrib
description: Guide and instructions for contributing to the Zabbix Terraform Provider project, adding new resources, and testing.
---

# Zabbix Terraform Provider Contribution Guide

Welcome! This guide outlines the development workflow, codebase architecture, and steps required to add new resources or modify existing ones in the Zabbix Terraform Provider.

---

## 🛠️ Development Prerequisites

1. **Go Environment:**
   This project uses **Go 1.22.6+** located locally under `/home/adi/go/bin/go`.
   To run commands, always use the absolute path or configure your PATH to include `/home/adi/go/bin`.

2. **Zabbix Server Version:**
   The provider is optimized to target **Zabbix 6.4 API**.
   * It uses `apiinfo.version` to verify compatibility.
   * Ensure any new API parameters you use are compliant with Zabbix 6.4 specs (such as `selectParentTemplates` and `interfaces` requirements).

---

## 📂 Codebase Architecture

* [main.go](file:///home/adi/terraform-provider-zabbix/main.go) – Plugin server entrypoint.
* [zabbix/client.go](file:///home/adi/terraform-provider-zabbix/zabbix/client.go) – Custom Zabbix JSON-RPC 2.0 API client. Handles authentication, RPC requests, and client structs.
* [zabbix/provider.go](file:///home/adi/terraform-provider-zabbix/zabbix/provider.go) – Configures provider inputs (URL, Credentials) and registers resources.
* [zabbix/resource_host_group.go](file:///home/adi/terraform-provider-zabbix/zabbix/resource_host_group.go) – Logic for the `zabbix_host_group` resource.
* [zabbix/resource_host.go](file:///home/adi/terraform-provider-zabbix/zabbix/resource_host.go) – Logic for the `zabbix_host` resource.

---

## 📝 Step-by-Step: Adding a New Resource

To add a new Zabbix resource (e.g. `zabbix_template` or `zabbix_user`):

### 1. Extend the Zabbix API Client
In [client.go](file:///home/adi/terraform-provider-zabbix/zabbix/client.go), add the struct definition and API helper methods for CRUD operations. For example, to manage user groups:
```go
type UserGroup struct {
	Usrgrpid string `json:"usrgrpid"`
	Name     string `json:"name"`
}

func (c *ZabbixClient) CreateUserGroup(name string) (string, error) {
	// Call "usergroup.create" RPC
}
```

### 2. Create the Resource Controller File
Create a new file `zabbix/resource_<name>.go` (e.g. `zabbix/resource_user_group.go`). Implement the CRUD schema using the Terraform Plugin SDKv2 template:
```go
package zabbix

import (
	"context"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func resourceUserGroup() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceUserGroupCreate,
		ReadContext:   resourceUserGroupRead,
		UpdateContext: resourceUserGroupUpdate,
		DeleteContext: resourceUserGroupDelete,
		Schema: map[string]*schema.Schema{
			"name": {
				Type:     schema.TypeString,
				Required: true,
			},
		},
	}
}
```

### 3. Register the Resource in the Provider
In [provider.go](file:///home/adi/terraform-provider-zabbix/zabbix/provider.go), add the new resource to the `ResourcesMap` array:
```go
		ResourcesMap: map[string]*schema.Resource{
			"zabbix_host_group": resourceHostGroup(),
			"zabbix_host":       resourceHost(),
			"zabbix_user_group": resourceUserGroup(), // Add this line
		},
```

### 4. Build and Verify
Run the compilation to ensure everything compiles cleanly:
```bash
/home/adi/go/bin/go mod tidy
/home/adi/go/bin/go build -o terraform-provider-zabbix
```

---

## 🧪 Testing Workflows

### Acceptance Tests
To run acceptance tests, configure these environment variables so the test suite can connect to a live Zabbix test instance:
```bash
export ZABBIX_URL="http://your-zabbix-host/zabbix/api_jsonrpc.php"
export ZABBIX_USERNAME="Admin"
export ZABBIX_PASSWORD="yourpassword"
```

Then run:
```bash
/home/adi/go/bin/go test ./zabbix -v
```

---

## 🚀 Git and Contribution Pipeline

1. **Create a branch:**
   ```bash
   git checkout -b feature/your-feature-name
   ```
2. **Commit changes:**
   Follow standard semantic commit styling (e.g. `feat: add zabbix_user_group resource`).
   ```bash
   git add .
   git commit -m "feat: add zabbix_user_group resource"
   ```
3. **Push and Open PR:**
   ```bash
   git push origin feature/your-feature-name
   ```
