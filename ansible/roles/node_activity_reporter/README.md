# node_activity_reporter Ansible role

Installs, configures, enables, and starts the Phase 1 host reporter. The role
does not create Kubernetes credentials or cluster resources.

Choose one installation source:

```yaml
- hosts: interactive_nodes
  become: true
  roles:
    - role: node_activity_reporter
      node_activity_reporter_install_method: binary
      node_activity_reporter_binary_src: ./dist/node-activity-reporter-linux-amd64
```

Alternatively set `node_activity_reporter_install_method: package` and provide
`node_activity_reporter_package_name` from a repository already trusted by the
host. Override `node_activity_reporter_config` as a complete mapping. Keep
`fail_closed: true`; Kubernetes mode also requires a pre-created NodeActivity,
CA file, and a rotating credential restricted to that resource's status.
