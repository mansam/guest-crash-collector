# guest-crash-collector

A CLI tool for gathering diagnostic context about guest OS crashes in KubeVirt VMs. It collects VM and VMI definitions, host kernel logs, virt-launcher pod logs, and optionally extracts Windows crash dumps — then packages everything into a tarball.

Assumes cluster-admin privileges.

## Usage

```
guest-crash-collector \
  -n <namespace> \
  -v <vm-name> \
  -a <crash-timestamp> \
  [-w <window>] \
  [--collect-dump]
```

### Example

```bash
guest-crash-collector \
  -n my-namespace \
  -v my-windows-vm \
  -a 2026-06-23T14:30:00Z \
  -w 30m \
  --collect-dump
```

## What it collects

1. **VirtualMachine YAML** — the VM definition
2. **VirtualMachineInstance YAML** — the running VMI (includes node placement, phase, etc.)
3. **Node dmesg** — kernel ring buffer from the host node, filtered to the time window around the crash
4. **Virt-launcher pod logs** — logs from all containers in the virt-launcher pod
5. **Windows crash dump** (opt-in via `--collect-dump`) — extracts `MEMORY.DMP` and minidumps from the VM's disk using libguestfs

Steps 1-4 are packaged into a `.tar.gz` archive. Crash dump files are saved as separate files alongside the archive (they can be several GB).

## Flags

| Flag | Short | Required | Default | Description |
|------|-------|----------|---------|-------------|
| `--namespace` | `-n` | yes | | VM namespace |
| `--vm` | `-v` | yes | | VM name |
| `--around` | `-a` | yes | | Crash timestamp (RFC 3339) |
| `--window` | `-w` | no | `30m` | Time window around the crash |
| `--collect-dump` | | no | `false` | Extract Windows crash dump from disk |
| `--disk` | | no | first PVC volume | VM volume name for the boot disk |
| `--debug-image` | | no | `ubi9/ubi-minimal` | Image for the dmesg debug pod |
| `--guestfs-image` | | no | `kubevirt/libguestfs-tools:latest` | Image for the guestfs pod |
| `--kubeconfig` | | no | `$KUBECONFIG` | Path to kubeconfig |

## How it works

- **Node detection**: reads `status.nodeName` from the VirtualMachineInstance. The VM must be running.
- **dmesg collection**: creates a temporary privileged pod on the target node that runs `chroot /host dmesg --since ... --until ...` to capture host kernel logs in the time window. The pod is cleaned up afterward.
- **Crash dump extraction**: creates a pod with `libguestfs-tools` on the same node as the VM, mounts the VM's PVC read-only, and uses `guestfish --ro --inspector` to find the Windows partition and extract dump files. Files are streamed to the local machine via the Kubernetes exec API. The pod is cleaned up afterward.

## Building

```
make build
```

Produces `bin/guest-crash-collector`.
