package host

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/softprobe/lisa/lisavm/internal/domain"
)

type Provisioner interface {
	CloneDisk(srcRootfs, dstDisk string) error
	InjectBootstrap(diskPath string, cfg domain.BootstrapConfig) error
	EnsureTAP(tapName, bridgeName string, uid, gid int) error
	DeleteTAP(tapName string) error
}

type LinuxProvisioner struct {
	gatewayCIDR string
}

func NewLinuxProvisioner(gatewayIP ...string) *LinuxProvisioner {
	gw := "172.16.0.1"
	if len(gatewayIP) > 0 && strings.TrimSpace(gatewayIP[0]) != "" {
		gw = strings.TrimSpace(gatewayIP[0])
	}
	cidr := gw
	if !strings.Contains(cidr, "/") {
		cidr = cidr + "/16"
	}
	return &LinuxProvisioner{gatewayCIDR: cidr}
}

func resolveTool(name string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, dir := range []string{"/sbin", "/usr/sbin", "/bin", "/usr/bin"} {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return name
}

func (p *LinuxProvisioner) CloneDisk(srcRootfs, dstDisk string) error {
	if _, err := os.Stat(srcRootfs); err != nil {
		return fmt.Errorf("source rootfs %s: %w", srcRootfs, err)
	}
	if err := os.MkdirAll(filepath.Dir(dstDisk), 0o700); err != nil {
		return fmt.Errorf("create disk dir: %w", err)
	}
	_ = os.Chmod(filepath.Dir(dstDisk), 0o700)
	tmpDisk := dstDisk + ".tmp"
	_ = os.Remove(tmpDisk)
	cmd := exec.Command("cp", "--reflink=auto", "--sparse=always", srcRootfs, tmpDisk)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmpDisk)
		return fmt.Errorf("clone disk: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(tmpDisk, 0o600); err != nil {
		_ = os.Remove(tmpDisk)
		return fmt.Errorf("chmod cloned disk: %w", err)
	}
	if err := os.Rename(tmpDisk, dstDisk); err != nil {
		_ = os.Remove(tmpDisk)
		return fmt.Errorf("rename cloned disk: %w", err)
	}
	return nil
}

func (p *LinuxProvisioner) InjectBootstrap(diskPath string, cfg domain.BootstrapConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bootstrap: %w", err)
	}
	data = append(data, '\n')

	tmpFile, err := os.CreateTemp("", "lisavm-bootstrap-*.json")
	if err != nil {
		return fmt.Errorf("create temp bootstrap file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("chmod temp bootstrap file: %w", err)
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp bootstrap file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp bootstrap file: %w", err)
	}

	debugfsBin := resolveTool("debugfs")
	_ = exec.Command(debugfsBin, "-w", "-R", "mkdir /etc", diskPath).Run()
	_ = exec.Command(debugfsBin, "-w", "-R", "mkdir /etc/lisavm", diskPath).Run()
	_ = exec.Command(debugfsBin, "-w", "-R", "sif /etc/lisavm mode 040700", diskPath).Run()
	_ = exec.Command(debugfsBin, "-w", "-R", "rm /etc/lisavm/bootstrap.json", diskPath).Run()

	writeCmd := exec.Command(debugfsBin, "-w", "-R", fmt.Sprintf("write %s /etc/lisavm/bootstrap.json", tmpPath), diskPath)
	if out, err := writeCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("debugfs write bootstrap.json: %w: %s", err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command(debugfsBin, "-w", "-R", "sif /etc/lisavm/bootstrap.json mode 0100600", diskPath).Run()

	statCmd := exec.Command(debugfsBin, "-R", "stat /etc/lisavm/bootstrap.json", diskPath)
	if out, err := statCmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "Inode:") {
		return fmt.Errorf("verify bootstrap.json on %s failed: %v %s", diskPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *LinuxProvisioner) ensureBridge(ipBin, bridgeName string) error {
	if err := exec.Command(ipBin, "link", "show", bridgeName).Run(); err != nil {
		if out, addErr := exec.Command(ipBin, "link", "add", "name", bridgeName, "type", "bridge").CombinedOutput(); addErr != nil {
			return fmt.Errorf("create bridge %s: %w: %s", bridgeName, addErr, strings.TrimSpace(string(out)))
		}
	}
	if p.gatewayCIDR != "" {
		addrOut, addrErr := exec.Command(ipBin, "-4", "addr", "show", "dev", bridgeName).CombinedOutput()
		if addrErr != nil || !strings.Contains(string(addrOut), p.gatewayCIDR) {
			if out, repErr := exec.Command(ipBin, "addr", "replace", p.gatewayCIDR, "dev", bridgeName).CombinedOutput(); repErr != nil {
				return fmt.Errorf("assign %s on %s: %w: %s", p.gatewayCIDR, bridgeName, repErr, strings.TrimSpace(string(out)))
			}
		}
	}
	if out, err := exec.Command(ipBin, "link", "set", "dev", bridgeName, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("bring up bridge %s: %w: %s", bridgeName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *LinuxProvisioner) EnsureTAP(tapName, bridgeName string, uid, gid int) error {
	ipBin := resolveTool("ip")
	if err := p.ensureBridge(ipBin, bridgeName); err != nil {
		return err
	}
	_ = exec.Command(ipBin, "link", "del", tapName).Run()
	args := []string{"tuntap", "add", "dev", tapName, "mode", "tap"}
	// Bind TAP ownership to the instance's jailer UID/GID so unprivileged Firecracker can open TUNSETIFF without CAP_NET_ADMIN.
	if os.Geteuid() == 0 && uid > 0 && gid > 0 {
		args = append(args, "user", strconv.Itoa(uid), "group", strconv.Itoa(gid))
	}
	if out, err := exec.Command(ipBin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("create tap %s: %w: %s", tapName, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(ipBin, "link", "set", tapName, "master", bridgeName).CombinedOutput(); err != nil {
		return fmt.Errorf("attach tap %s to %s: %w: %s", tapName, bridgeName, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(ipBin, "link", "set", tapName, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("bring up tap %s: %w: %s", tapName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *LinuxProvisioner) DeleteTAP(tapName string) error {
	if tapName == "" {
		return nil
	}
	ipBin := resolveTool("ip")
	if err := exec.Command(ipBin, "link", "show", tapName).Run(); err != nil {
		return nil
	}
	if out, err := exec.Command(ipBin, "link", "del", tapName).CombinedOutput(); err != nil {
		return fmt.Errorf("delete tap %s: %w: %s", tapName, err, strings.TrimSpace(string(out)))
	}
	return nil
}
