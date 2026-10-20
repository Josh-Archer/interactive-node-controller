# node_activity_reporter Ansible role

Installs, configures, enables, and starts the Phase 1 host reporter. The role
does not create Kubernetes credentials or cluster resources.

Choose one installation source:

### 1. Version-pinned GitHub release download (recommended)

Download and verify a version-pinned binary directly from GitHub Releases:

```yaml
- hosts: interactive_nodes
  become: true
  vars:
    # renovate: datasource=github-releases depName=Josh-Archer/interactive-node-controller
    node_activity_reporter_version: "v0.1.5"
    # Pinned SHA-256 checksum from published checksums.txt:
    node_activity_reporter_checksum: "40b00ee4215d468c019ebc41d944c630baaf39ffe7364353274d5a44dafa22b5"
  roles:
    - role: node_activity_reporter
      node_activity_reporter_install_method: download
      node_activity_reporter_version: "{{ node_activity_reporter_version }}"
      node_activity_reporter_checksum: "{{ node_activity_reporter_checksum }}"
```

### 2. Pre-supplied local binary

```yaml
- hosts: interactive_nodes
  become: true
  roles:
    - role: node_activity_reporter
      node_activity_reporter_install_method: binary
      node_activity_reporter_binary_src: ./dist/node-activity-reporter-linux-amd64
```

### 3. Package source

Alternatively set `node_activity_reporter_install_method: package` and provide
`node_activity_reporter_package_name` from a repository already trusted by the
host. Override `node_activity_reporter_config` as a complete mapping. Keep
`fail_closed: true`; Kubernetes mode also requires a pre-created NodeActivity,
CA file, and a rotating credential restricted to that resource's status.
