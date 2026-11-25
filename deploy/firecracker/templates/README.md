# Portable lisavm KVM Host Deployment

Deploy `lisavm` and its Firecracker MicroVM runtime on any Linux host with `/dev/kvm`.

## Prerequisites

- Linux x86_64 or arm64 host with `/dev/kvm` available.
- Root access (`sudo`) for bridge/TAP networking and Firecracker execution.
- Release artifacts from a `lisavm-v*` release (or generated locally with `../package-release.sh`).

## Host Templates

| File | Purpose |
|------|---------|
| `lisavm.service` | systemd unit running `/usr/local/bin/lisavm` |
| `lisavm.env.example` | Environment configuration loaded from `/etc/lisavm/lisavm.env` |
| `Caddyfile.example` | Automatic TLS reverse proxy when port 443 is publicly reachable |
| `cloudflared-config.yml.example` | Cloudflare Tunnel ingress when no public IP or inbound port 443 is exposed |
| `cloud-init-kvm.yaml` | Cloud-init bootstrap for GCP nested-virt KVM, AWS `.metal`, or generic KVM VMs |

## 1. GCP Nested Virtualization (Compute Engine)

Create a Compute Engine VM with nested virtualization enabled on Ubuntu 24.04 LTS:

```bash
gcloud compute instances create lisavm-host \
  --zone=us-central1-a \
  --machine-type=n2-standard-4 \
  --enable-nested-virtualization \
  --image-family=ubuntu-2404-lts-amd64 \
  --image-project=ubuntu-os-cloud \
  --boot-disk-size=50GB \
  --metadata-from-file=user-data=cloud-init-kvm.yaml
```

Verify `/dev/kvm` and `lisavm.service` after boot:

```bash
ls -l /dev/kvm
sudo systemctl status lisavm.service
curl -fsS http://127.0.0.1:7700/health
```

## 2. AWS Bare-Metal (`.metal`)

Launch an AWS bare-metal instance (`c6i.metal` for `amd64` or `c7g.metal` for `arm64`) with Ubuntu 24.04 LTS and pass `cloud-init-kvm.yaml` as EC2 user data:

```bash
aws ec2 run-instances \
  --instance-type c6i.metal \
  --image-id resolve:ssm:/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id \
  --user-data file://cloud-init-kvm.yaml
```

## 3. Generic Linux KVM Host (Manual Install)

1. Verify `/dev/kvm` exists on the host.
2. Copy `lisavm-linux-<arch>` to `/usr/local/bin/lisavm` (`chmod 0755`).
3. Decompress `rootfs-<arch>.ext4.zst` to `/var/lib/lisavm/images/rootfs.ext4` (`zstd -d`).
4. Run `sudo ../setup-kvm-host.sh` to install Firecracker, guest kernel `vmlinux`, and `lisabr0` NAT networking.
5. Configure `/etc/lisavm/lisavm.env` from `lisavm.env.example` and set a strong `LISAVM_API_TOKEN`.
6. Install `lisavm.service` to `/etc/systemd/system/lisavm.service` and start it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now lisavm.service
```

## 4. Expose TLS Endpoint

- **Public IP with port 80/443:** Install Caddy and copy `Caddyfile.example` to `/etc/caddy/Caddyfile` with your domain name.
- **Private host / behind NAT:** Install `cloudflared`, create a tunnel, and copy `cloudflared-config.yml.example` to `/etc/cloudflared/config.yml`.
