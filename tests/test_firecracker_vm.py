from __future__ import annotations

import json
import os
import re
import shutil
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.request
from contextlib import asynccontextmanager
from pathlib import Path

import pytest
import yaml
from fastapi.testclient import TestClient

from lisa import app as lisa_app

REPO_ROOT = Path(__file__).resolve().parents[1]
DEPLOY_DIR = REPO_ROOT / "deploy" / "firecracker"
LISAVM_DIR = REPO_ROOT / "lisavm"


def _tool_env(extra_bin: Path | None = None) -> dict[str, str]:
    env = os.environ.copy()
    parts = [str(extra_bin)] if extra_bin is not None else []
    parts.extend([env.get("PATH", ""), "/sbin", "/usr/sbin"])
    env["PATH"] = ":".join(part for part in parts if part)
    return env


def _make_ext4_image(path: Path, size_mb: int = 8) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("wb") as handle:
        handle.truncate(size_mb * 1024 * 1024)
    subprocess.run(
        ["mkfs.ext4", "-F", str(path)],
        check=True,
        capture_output=True,
        text=True,
        env=_tool_env(),
    )


def _free_port() -> int:
    import socket

    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def _http_json(
    url: str,
    *,
    method: str = "GET",
    token: str | None = None,
    payload: dict | None = None,
    timeout: float = 10.0,
) -> tuple[int, object]:
    body = None if payload is None else json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=body, method=method)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    if token is not None:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8")
            return resp.status, json.loads(raw) if raw else None
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8")
        return exc.code, json.loads(raw) if raw else None


@pytest.fixture(scope="module")
def lisavm_binary(tmp_path_factory: pytest.TempPathFactory) -> Path:
    assert shutil.which("go"), "go toolchain is required to build and test lisavm"
    out = tmp_path_factory.mktemp("lisavm-bin") / "lisavm"
    subprocess.run(
        ["go", "build", "-o", str(out), "./cmd/lisavm"],
        cwd=LISAVM_DIR,
        check=True,
        capture_output=True,
        text=True,
    )
    return out


def test_lisavm_go_test_suite_passes() -> None:
    assert shutil.which("go"), "go toolchain is required to run lisavm tests"
    result = subprocess.run(
        ["go", "test", "-race", "./..."],
        cwd=LISAVM_DIR,
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, f"go test failed:\n{result.stdout}\n{result.stderr}"


def test_guest_bootstrap_writes_restrictive_permissions_and_redacts_invalid_env_errors(
    tmp_path: Path,
) -> None:
    etc_dir = tmp_path / "etc-lisavm"
    etc_dir.mkdir(mode=0o755)
    bootstrap_json = etc_dir / "bootstrap.json"
    env_out = etc_dir / "lisa.env"
    plugins_dir = etc_dir / "plugins"

    subprocess.run(
        [str(DEPLOY_DIR / "guest-bootstrap.sh")],
        check=True,
        capture_output=True,
        text=True,
        env={
            **os.environ,
            "LISAVM_BOOTSTRAP_JSON": str(bootstrap_json),
            "LISAVM_ENV_OUT": str(env_out),
            "LISAVM_PLUGINS_DIR": str(plugins_dir),
        },
    )
    assert stat.S_IMODE(etc_dir.stat().st_mode) == 0o700
    assert stat.S_IMODE(plugins_dir.stat().st_mode) == 0o700
    assert stat.S_IMODE(env_out.stat().st_mode) == 0o600
    assert stat.S_IMODE((plugins_dir / "mcp-servers.json").stat().st_mode) == 0o600

    bootstrap_json.write_text(
        json.dumps(
            {
                "env": {
                    "GOOGLE_API_KEY": "secret-gemini-key",
                    "LISA_SERVICE_TOKEN": "secret-service-token",
                },
                "plugins": {
                    "blackbox": {
                        "transport": "http",
                        "url": "https://blackbox.example/mcp",
                        "headers": {"Authorization": "Bearer secret-bb-token"},
                    }
                },
            }
        ),
        encoding="utf-8",
    )
    bootstrap_json.chmod(0o644)

    subprocess.run(
        [str(DEPLOY_DIR / "guest-bootstrap.sh")],
        check=True,
        capture_output=True,
        text=True,
        env={
            **os.environ,
            "LISAVM_BOOTSTRAP_JSON": str(bootstrap_json),
            "LISAVM_ENV_OUT": str(env_out),
            "LISAVM_PLUGINS_DIR": str(plugins_dir),
        },
    )

    assert stat.S_IMODE(etc_dir.stat().st_mode) == 0o700
    assert stat.S_IMODE(plugins_dir.stat().st_mode) == 0o700
    assert stat.S_IMODE(bootstrap_json.stat().st_mode) == 0o600
    assert stat.S_IMODE(env_out.stat().st_mode) == 0o600
    mcp_file = plugins_dir / "mcp-servers.json"
    assert stat.S_IMODE(mcp_file.stat().st_mode) == 0o600
    assert "GOOGLE_API_KEY=secret-gemini-key" in env_out.read_text(encoding="utf-8")
    assert (
        json.loads(mcp_file.read_text(encoding="utf-8"))["blackbox"]["headers"]["Authorization"]
        == "Bearer secret-bb-token"
    )

    bad_secret = "never-print-this-secret-value"
    bootstrap_json.write_text(
        json.dumps({"env": {"BAD-KEY": bad_secret}}),
        encoding="utf-8",
    )
    failed = subprocess.run(
        [str(DEPLOY_DIR / "guest-bootstrap.sh")],
        check=False,
        capture_output=True,
        text=True,
        env={
            **os.environ,
            "LISAVM_BOOTSTRAP_JSON": str(bootstrap_json),
            "LISAVM_ENV_OUT": str(env_out),
            "LISAVM_PLUGINS_DIR": str(plugins_dir),
        },
    )
    assert failed.returncode != 0
    assert bad_secret not in failed.stdout
    assert bad_secret not in failed.stderr


def test_lisavm_hides_secrets_from_meta_and_api_with_restrictive_modes(
    tmp_path: Path, lisavm_binary: Path
) -> None:
    root = tmp_path / "lisavm-root"
    images = root / "images"
    images.mkdir(parents=True)
    rootfs = images / "rootfs.ext4"
    _make_ext4_image(rootfs)
    kernel = root / "vmlinux"
    kernel.write_bytes(b"fake-kernel")

    port = _free_port()
    token = "api-secret-token"
    proc = subprocess.Popen(
        [str(lisavm_binary)],
        env={
            **_tool_env(),
            "LISAVM_ROOT": str(root),
            "LISAVM_ROOTFS_PATH": str(rootfs),
            "LISAVM_KERNEL_PATH": str(kernel),
            "LISAVM_LISTEN_ADDR": f"127.0.0.1:{port}",
            "LISAVM_API_TOKEN": token,
        },
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        base_url = f"http://127.0.0.1:{port}"
        for _ in range(50):
            try:
                status, _ = _http_json(f"{base_url}/health")
                if status == 200:
                    break
            except Exception:
                time.sleep(0.05)
        else:
            pytest.fail("lisavm did not become healthy")

        secret_env = "secret-gemini-value-xyz"
        secret_plugin = "secret-blackbox-bearer-xyz"
        status, created = _http_json(
            f"{base_url}/v1/instances",
            method="POST",
            token=token,
            payload={
                "owner_id": "11111111-1111-1111-1111-111111111111",
                "name": "secret-check",
                "idle_timeout_sec": 60,
                "boot": False,
                "env": {"GEMINI_API_KEY": secret_env},
                "plugins": {
                    "blackbox": {
                        "transport": "http",
                        "url": "https://bb.internal/mcp",
                        "headers": {"Authorization": f"Bearer {secret_plugin}"},
                    }
                },
            },
        )
        assert status == 201, created
        assert isinstance(created, dict)
        inst_id = created["id"]

        _, listed = _http_json(f"{base_url}/v1/instances", token=token)
        _, fetched = _http_json(f"{base_url}/v1/instances/{inst_id}", token=token)
        _, patched = _http_json(
            f"{base_url}/v1/instances/{inst_id}",
            method="PATCH",
            token=token,
            payload={"idle_timeout_sec": 120},
        )
        for view in (created, listed, fetched, patched):
            serialized = json.dumps(view)
            assert secret_env not in serialized
            assert secret_plugin not in serialized

        inst_dir = root / "instances" / inst_id
        meta_path = inst_dir / "meta.json"
        disk_path = inst_dir / "disk.ext4"
        assert stat.S_IMODE(inst_dir.stat().st_mode) == 0o700
        assert stat.S_IMODE(meta_path.stat().st_mode) == 0o600
        assert stat.S_IMODE(disk_path.stat().st_mode) == 0o600
        meta_text = meta_path.read_text(encoding="utf-8")
        assert secret_env not in meta_text
        assert secret_plugin not in meta_text

        stat_out = subprocess.run(
            ["debugfs", "-R", "stat /etc/lisavm/bootstrap.json", str(disk_path)],
            check=True,
            capture_output=True,
            text=True,
            env=_tool_env(),
        ).stdout
        assert "Mode:  0600" in stat_out

        cat = subprocess.run(
            ["debugfs", "-R", "cat /etc/lisavm/bootstrap.json", str(disk_path)],
            check=True,
            capture_output=True,
            text=True,
            env=_tool_env(),
        )
        bootstrapped = json.loads(cat.stdout)
        assert bootstrapped["env"]["GEMINI_API_KEY"] == secret_env
        assert bootstrapped["plugins"]["blackbox"]["headers"]["Authorization"] == (
            f"Bearer {secret_plugin}"
        )
    finally:
        proc.terminate()
        proc.wait(timeout=5)


def test_lisavm_invokes_jailer_instead_of_raw_firecracker(
    tmp_path: Path, lisavm_binary: Path
) -> None:
    root = tmp_path / "lisavm-root"
    images = root / "images"
    images.mkdir(parents=True)
    rootfs = images / "rootfs.ext4"
    _make_ext4_image(rootfs)
    kernel = root / "vmlinux"
    kernel.write_bytes(b"fake-kernel")

    stub_bin = tmp_path / "stub-bin"
    stub_bin.mkdir()
    capture_dir = tmp_path / "captured"
    capture_dir.mkdir()

    ip_log = capture_dir / "ip_calls.log"
    (stub_bin / "ip").write_text(
        f"""#!{sys.executable}
import sys
from pathlib import Path
with Path({str(ip_log)!r}).open("a") as f:
    f.write(" ".join(sys.argv[1:]) + "\\n")
if sys.argv[1:4] == ["link", "show", "lisabr0"]:
    sys.exit(1)
sys.exit(0)
""",
        encoding="utf-8",
    )
    (stub_bin / "ip").chmod(0o755)

    (stub_bin / "firecracker").write_text(
        f"#!/usr/bin/env bash\ntouch {capture_dir}/raw_firecracker_called\nexit 1\n",
        encoding="utf-8",
    )
    (stub_bin / "firecracker").chmod(0o755)

    fake_jailer = f"""#!{sys.executable}
import json, socketserver, sys, http.server
from pathlib import Path

capture = Path({str(capture_dir)!r})
argv = sys.argv[1:]
(capture / "jailer_argv.json").write_text(json.dumps(argv))

def flag(name):
    idx = argv.index(name)
    return argv[idx + 1]

inst_id = flag("--id")
chroot_base = Path(flag("--chroot-base-dir"))
exec_file = Path(flag("--exec-file"))
jail_root = chroot_base / exec_file.name / inst_id / "root"
jail_root.mkdir(parents=True, exist_ok=True)
(capture / "staged.json").write_text(json.dumps({{
    "disk_exists": (jail_root / "disk.ext4").exists(),
    "kernel_exists": (jail_root / "vmlinux").exists(),
    "disk_mode": oct((jail_root / "disk.ext4").stat().st_mode & 0o777)
    if (jail_root / "disk.ext4").exists() else None,
}}))

sep = argv.index("--")
fc_args = argv[sep + 1:]
sock_arg = fc_args[fc_args.index("--api-sock") + 1]
sock_path = jail_root / sock_arg.lstrip("/")
if sock_path.exists():
    sock_path.unlink()

api_calls = []
class Handler(http.server.BaseHTTPRequestHandler):
    def do_PUT(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length).decode("utf-8")) if length else {{}}
        api_calls.append({{"method": "PUT", "path": self.path, "body": body}})
        (capture / "api_calls.json").write_text(json.dumps(api_calls))
        self.send_response(204)
        self.end_headers()
    def log_message(self, *_args):
        pass

class UnixHTTPServer(socketserver.UnixStreamServer):
    def server_bind(self):
        import os
        os.chdir(jail_root)
        self.socket.bind(sock_path.name)
        self.server_name = "localhost"
        self.server_port = 0

with UnixHTTPServer(str(sock_path), Handler) as srv:
    srv.serve_forever()
"""
    (stub_bin / "jailer").write_text(fake_jailer, encoding="utf-8")
    (stub_bin / "jailer").chmod(0o755)

    port = _free_port()
    token = "api-secret-token"
    proc = subprocess.Popen(
        [str(lisavm_binary)],
        env={
            **_tool_env(stub_bin),
            "LISAVM_ROOT": str(root),
            "LISAVM_ROOTFS_PATH": str(rootfs),
            "LISAVM_KERNEL_PATH": str(kernel),
            "LISAVM_FIRECRACKER_BIN": str(stub_bin / "firecracker"),
            "LISAVM_JAILER_BIN": str(stub_bin / "jailer"),
            "LISAVM_LISTEN_ADDR": f"127.0.0.1:{port}",
            "LISAVM_API_TOKEN": token,
        },
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        base_url = f"http://127.0.0.1:{port}"
        for _ in range(50):
            try:
                status, _ = _http_json(f"{base_url}/health")
                if status == 200:
                    break
            except Exception:
                time.sleep(0.05)

        status, created = _http_json(
            f"{base_url}/v1/instances",
            method="POST",
            token=token,
            payload={
                "owner_id": "11111111-1111-1111-1111-111111111111",
                "name": "jailer-check",
                "idle_timeout_sec": 60,
                "boot": False,
            },
        )
        assert status == 201
        inst_id = created["id"]

        # Wake stops at WaitHealthy without root TAP routing; cancel once InstanceStart lands.
        try:
            _http_json(
                f"{base_url}/v1/instances/{inst_id}/wake",
                method="POST",
                token=token,
                timeout=1.5,
            )
        except Exception:
            pass

        assert not (capture_dir / "raw_firecracker_called").exists()
        assert (capture_dir / "jailer_argv.json").exists(), "jailer was never invoked"
        argv = json.loads((capture_dir / "jailer_argv.json").read_text(encoding="utf-8"))
        assert "--id" in argv and inst_id in argv
        assert "--exec-file" in argv
        assert "--uid" in argv and "--gid" in argv
        assert int(argv[argv.index("--uid") + 1]) >= 200000
        assert "--chroot-base-dir" in argv
        assert "--" in argv

        ip_commands = ip_log.read_text(encoding="utf-8")
        assert "link add name lisabr0 type bridge" in ip_commands
        assert "addr replace 172.16.0.1/16 dev lisabr0" in ip_commands

        staged = json.loads((capture_dir / "staged.json").read_text(encoding="utf-8"))
        assert staged["disk_exists"] is True
        assert staged["kernel_exists"] is True
        assert staged["disk_mode"] == "0o600"

        deadline = time.time() + 2.0
        while time.time() < deadline and not (capture_dir / "api_calls.json").exists():
            time.sleep(0.05)
        calls = {
            item["path"]: item["body"]
            for item in json.loads((capture_dir / "api_calls.json").read_text(encoding="utf-8"))
        }
        assert calls["/boot-source"]["kernel_image_path"] == "/vmlinux"
        assert calls["/drives/rootfs"]["path_on_host"] == "/disk.ext4"
    finally:
        proc.terminate()
        proc.wait(timeout=5)


def test_persistent_bridge_nat_networking_script_and_reboot_recovery(tmp_path: Path) -> None:
    network_script = DEPLOY_DIR / "lisavm-network.sh"
    assert network_script.is_file()
    network_unit = DEPLOY_DIR / "templates" / "lisavm-network.service"
    assert network_unit.is_file()

    lisavm_unit = (DEPLOY_DIR / "templates" / "lisavm.service").read_text(encoding="utf-8")
    assert "lisavm-network.service" in lisavm_unit
    assert "Requires=lisavm-network.service" in lisavm_unit
    cloud_init = (DEPLOY_DIR / "templates" / "cloud-init-kvm.yaml").read_text(encoding="utf-8")
    assert "lisavm-network.service" in cloud_init
    setup_script = (DEPLOY_DIR / "setup-kvm-host.sh").read_text(encoding="utf-8")
    assert "lisavm-network.service" in setup_script

    stub_bin = tmp_path / "net-bin"
    stub_bin.mkdir()
    log_file = tmp_path / "net-calls.log"
    state_dir = tmp_path / "net-state"
    state_dir.mkdir()
    sysctl_file = tmp_path / "99-lisavm.conf"

    (stub_bin / "ip").write_text(
        f"""#!{sys.executable}
import sys
from pathlib import Path
log = Path({str(log_file)!r})
state = Path({str(state_dir)!r})
args = sys.argv[1:]
with log.open("a") as f:
    f.write("ip " + " ".join(args) + "\\n")
if args[:2] == ["link", "show"]:
    sys.exit(0 if (state / f"link_{{args[2]}}").exists() else 1)
if args[:3] == ["link", "add", "name"]:
    (state / f"link_{{args[3]}}").write_text("up")
    sys.exit(0)
if args[:3] == ["-4", "addr", "show"]:
    if (state / "addr").exists():
        print((state / "addr").read_text())
        sys.exit(0)
    sys.exit(1)
if args[:2] == ["addr", "replace"]:
    (state / "addr").write_text(args[2])
    sys.exit(0)
if args[:3] == ["-4", "route", "show"]:
    print("default via 10.0.0.1 dev ens4")
    sys.exit(0)
sys.exit(0)
""",
        encoding="utf-8",
    )
    (stub_bin / "ip").chmod(0o755)
    (stub_bin / "iptables").write_text(
        f"""#!{sys.executable}
import hashlib, sys
from pathlib import Path
log = Path({str(log_file)!r})
state = Path({str(state_dir)!r})
args = sys.argv[1:]
with log.open("a") as f:
    f.write("iptables " + " ".join(args) + "\\n")
if "-C" in args:
    key = hashlib.sha256(" ".join(args).replace(" -C ", " ").encode()).hexdigest()
    sys.exit(0 if (state / f"ipt_{{key}}").exists() else 1)
if "-A" in args:
    key = hashlib.sha256(" ".join(args).replace(" -A ", " ").encode()).hexdigest()
    (state / f"ipt_{{key}}").write_text("1")
sys.exit(0)
""",
        encoding="utf-8",
    )
    (stub_bin / "iptables").chmod(0o755)
    (stub_bin / "sysctl").write_text(
        f'#!/usr/bin/env bash\necho "sysctl $*" >> {log_file}\n', encoding="utf-8"
    )
    (stub_bin / "sysctl").chmod(0o755)

    env = {
        **_tool_env(stub_bin),
        "LISAVM_BRIDGE": "lisabr0",
        "LISAVM_BRIDGE_CIDR": "172.16.0.1/16",
        "LISAVM_SYSCTL_FILE": str(sysctl_file),
    }
    subprocess.run([str(network_script)], check=True, env=env)
    subprocess.run([str(network_script)], check=True, env=env)

    lines = log_file.read_text(encoding="utf-8").splitlines()
    assert sum(1 for line in lines if line == "ip link add name lisabr0 type bridge") == 1
    assert sum(1 for line in lines if " -A " in line and "MASQUERADE" in line) == 1
    assert sum(1 for line in lines if " -C " in line and "MASQUERADE" in line) == 2
    assert "net.ipv4.ip_forward = 1" in sysctl_file.read_text(encoding="utf-8")

    shutil.rmtree(state_dir)
    state_dir.mkdir()
    subprocess.run([str(network_script)], check=True, env=env)

    after_reboot = log_file.read_text(encoding="utf-8").splitlines()
    assert sum(1 for line in after_reboot if line == "ip link add name lisabr0 type bridge") == 2
    assert sum(1 for line in after_reboot if " -A " in line and "MASQUERADE" in line) == 2
    assert (state_dir / "link_lisabr0").exists()
    assert (state_dir / "addr").read_text(encoding="utf-8") == "172.16.0.1/16"


def test_guest_rootfs_builds_and_serves_production_frontend_assets(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    rootfs_dockerfile = (DEPLOY_DIR / "Dockerfile.rootfs").read_text(encoding="utf-8")
    init_script = (DEPLOY_DIR / "init.sh").read_text(encoding="utf-8")

    assert re.search(r"COPY --from=lisa-frontend\s+/ui/dist\s+/app/static", rootfs_dockerfile)
    assert "postgresql" in rootfs_dockerfile
    assert "pgmq" in rootfs_dockerfile
    assert "provision_lisa.py" in rootfs_dockerfile
    assert "provision_lisa.py" in init_script
    assert "LISA_MCP_SERVERS_PATH" in init_script

    stage_tag = "lisavm-frontend-stage-test:local"
    subprocess.run(
        [
            "docker",
            "build",
            "--target",
            "lisa-frontend",
            "-f",
            str(DEPLOY_DIR / "Dockerfile.rootfs"),
            "-t",
            stage_tag,
            str(REPO_ROOT),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    static_dir = tmp_path / "static"
    static_dir.mkdir()
    cid = subprocess.run(
        ["docker", "create", stage_tag],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    try:
        subprocess.run(
            ["docker", "cp", f"{cid}:/ui/dist/.", str(static_dir)],
            check=True,
            capture_output=True,
            text=True,
        )
    finally:
        subprocess.run(["docker", "rm", "-f", cid], check=False, capture_output=True)

    @asynccontextmanager
    async def no_lifespan(_app):
        yield

    monkeypatch.setattr(lisa_app.app.router, "lifespan_context", no_lifespan)
    monkeypatch.setenv("LISA_STATIC_DIR", str(static_dir))

    with TestClient(lisa_app.app) as client:
        index_resp = client.get("/")
        assert index_resp.status_code == 200
        assert '<div id="root"></div>' in index_resp.text
        match = re.search(r'(/assets/[^"\' >]+\.js)', index_resp.text)
        assert match is not None, f"expected built JS bundle in index.html: {index_resp.text[:300]}"
        js_resp = client.get(match.group(1))
        assert js_resp.status_code == 200
        assert len(js_resp.content) > 100
        assert "/v1" in js_resp.text


def test_supported_architectures_align_across_release_templates_and_docs(tmp_path: Path) -> None:
    workflow = yaml.safe_load(
        (REPO_ROOT / ".github" / "workflows" / "lisavm-release.yml").read_text(encoding="utf-8")
    )
    binary_arches = set(workflow["jobs"]["build-binaries"]["strategy"]["matrix"]["arch"])
    rootfs_steps_text = json.dumps(workflow["jobs"]["build-rootfs"]["steps"])
    rootfs_arches = set(re.findall(r"rootfs-(amd64|arm64)\.ext4\.zst", rootfs_steps_text))

    assert binary_arches == rootfs_arches == {"amd64"}

    cloud_init = (DEPLOY_DIR / "templates" / "cloud-init-kvm.yaml").read_text(encoding="utf-8")
    assert "arm64" not in cloud_init and "aarch64" not in cloud_init

    templates_readme = (DEPLOY_DIR / "templates" / "README.md").read_text(encoding="utf-8")
    assert "arm64" not in templates_readme and "c7g.metal" not in templates_readme

    setup_script = (DEPLOY_DIR / "setup-kvm-host.sh").read_text(encoding="utf-8")
    assert "aarch64" not in setup_script and "arm64" not in setup_script

    out_dir = tmp_path / "dist"
    good_pkg = subprocess.run(
        [str(DEPLOY_DIR / "package-release.sh")],
        env={
            **os.environ,
            "TARGET_ARCH": "amd64",
            "SKIP_ROOTFS": "1",
            "OUT_DIR": str(out_dir),
        },
        capture_output=True,
        text=True,
        check=False,
    )
    assert good_pkg.returncode == 0, good_pkg.stderr
    assert (out_dir / "lisavm-linux-amd64").is_file()
    assert (out_dir / "setup-kvm-host.sh").is_file()
    assert (out_dir / "lisavm-network.sh").is_file()
    assert (out_dir / "lisavm-network.service").is_file()
    subprocess.run(["sha256sum", "-c", "SHA256SUMS"], cwd=out_dir, check=True, capture_output=True)

    bad_pkg = subprocess.run(
        [str(DEPLOY_DIR / "package-release.sh")],
        env={**os.environ, "TARGET_ARCH": "arm64", "SKIP_ROOTFS": "1", "OUT_DIR": str(tmp_path / "bad")},
        capture_output=True,
        text=True,
        check=False,
    )
    assert bad_pkg.returncode != 0
