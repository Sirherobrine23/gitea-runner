# macOS VM executor

The runner can execute a job in an ephemeral macOS VM by registering a
`macos-vm` label:

```yaml
runner:
  labels:
    - macos-latest:macos-vm://registry.example.com/macos-15.2-xcode-26.1
```

The VM is created and managed by `runner-executor-macos`, a dedicated Swift
binary built on Apple Virtualization.framework. The source VM must include the
runner guest agent.

## Configuration

```yaml
macos_vm:
  executor_path: runner-executor-macos
  workdir_parent: /Users/admin/runner
  cpu: 0
  memory: 0
  boot_timeout: 5m
```

`workdir_parent` is the guest directory under which job workspaces are created.
`executor_path` is the binary to invoke; an empty value uses PATH lookup.

## Executor CLI

The runner uses a narrow, runner-oriented command set:

```text
runner-executor-macos provision <image> <name> [--cpu N] [--memory MB]
runner-executor-macos start <name> [--no-graphics]
runner-executor-macos exec <name> [--stdin] <argv...>
runner-executor-macos stop <name> [--timeout N]
runner-executor-macos destroy <name>
runner-executor-macos list --format json
runner-executor-macos version
```

`list --format json` returns:

```json
[
  {
    "name": "GITEA-MACOS-VM-<runner-uuid>-...",
    "state": "stopped",
    "running": false,
    "accessed_at": "2026-09-05T12:00:00Z"
  }
]
```

## Integration test

The end-to-end test is opt-in because it provisions and boots a real VM:

```bash
MACOS_VM_TEST_IMAGE=/path/or/reference \
  go test -tags macos_vm_integration ./act/container -run '^TestMacOSVMEnvironmentIntegration$' -v
```

The test skips when `MACOS_VM_TEST_IMAGE` is unset.

## Orphaned VM cleanup

Job VM names start with `GITEA-MACOS-VM-<runner-uuid>-`. While the runner is
idle, it lists local VMs and destroys the non-running ones whose name has this
runner's UUID and whose access time is older than
`runner.workdir_cleanup_age`.

## Service containers

Service containers are intentionally rejected for macOS VM jobs today. Docker
service containers live on a Docker network and are reached by workflow service
id, which a macOS VM cannot resolve.

Possible future designs:

1. **Host-published Docker services + guest host mapping** — publish each
   service port on the runner host and inject `<service-id> -> <host-ip>` into
   the VM through the guest agent.
2. **macOS VM services** — run each service as another VM; this requires
   service discovery and is much heavier than Docker containers.
3. **Environment-based discovery** — require workflows to read service
   addresses from env vars instead of hostnames, breaking GitHub Actions
   compatibility.

Option 1 is the most compatible direction once the guest agent and host network
configuration are stable.
