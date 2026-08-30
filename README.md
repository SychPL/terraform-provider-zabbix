# Terraform Provider for Zabbix (6.4 API)

A custom Terraform provider for managing [Zabbix](https://www.zabbix.com/) resources. This provider is specifically designed and optimized to interface with **Zabbix API version 6.4.x**.

---

## Features

* **`zabbix_host_group`**: Manage Zabbix host groups.
* **`zabbix_host`**: Manage Zabbix hosts, configure agent interfaces (IP/Port), and link templates.
* **Automatic Version Check**: Provider automatically connects and verifies Zabbix API compatibility on startup.

---

## Requirements

* [Terraform](https://www.terraform.io) 1.0+
* [Go](https://golang.org) 1.22+ (to compile from source)
* A Zabbix Server instance running version **6.4.x**

---

## Building and Installing

### 1. Build the Provider Binary
To compile the provider locally, run the build command:
```bash
go build -o terraform-provider-zabbix
```

### 2. Configure Local Developer Override (Recommended for Dev)
To test this local provider with Terraform without publishing it to the registry, set up a **developer override** in your local `~/.terraformrc` file:

```hcl
provider_installation {
  dev_overrides {
    "adi/zabbix" = "/home/adi/terraform-provider-zabbix"
  }
  
  # For all other providers, install directly from registry
  direct {}
}
```

With this configuration, Terraform will automatically look for the compiled binary at `/home/adi/terraform-provider-zabbix` whenever you reference the provider `adi/zabbix`.

---

## Example Usage

Create a `main.tf` file:

```hcl
terraform {
  required_providers {
    zabbix = {
      source = "adi/zabbix"
    }
  }
}

# Configure the Zabbix Provider
provider "zabbix" {
  url      = "http://your-zabbix-server/zabbix/api_jsonrpc.php"
  username = "Admin"
  password = "zabbix_password"
}

# Create a Host Group
resource "zabbix_host_group" "servers" {
  name = "Linux Web Servers"
}

# Create a Host and assign it to the Group
resource "zabbix_host" "web_node_01" {
  host   = "web-server-production-01"
  groups = [zabbix_host_group.servers.id]
  ip     = "192.168.1.100"
  port   = "10050"
  
  # Optionally link Zabbix Template IDs
  # templates = ["10001", "10186"]
}
```

Initialize and apply:
```bash
terraform init
terraform apply
```

---

## Contributing

For guidelines on coding standards, codebase structure, and adding new Zabbix resources, please read the [zabbix-provider-contrib skill guide](file://.agents/skills/zabbix-provider-contrib/SKILL.md) in this repository.

---

## License

This project is licensed under the MIT License - see the [LICENSE](file://LICENSE) file for details.
