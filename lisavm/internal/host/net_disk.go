package host

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/softprobe/lisa/lisavm/internal/domain"
)

type Provisioner interface {
	CloneDisk(srcRootfs, dstDisk string) error
	InjectBootstrap(diskPath string, cfg domain.BootstrapConfig) error
	EnsureTAP(tapName, bridgeName string) error
	DeleteTAP(tapName string) error
}

type LinuxProvisioner struct{}

func NewLinuxProvisioner() *LinuxProvisioner {
	return &LinuxProvisioner{}
}

func (p *LinuxProvisioner) CloneDisk(srcRootfs, dstDisk string) error {
	if _, err := os.Stat(srcRootfs); err != nil {
		return fmt.Errorf("source rootfs %s: %w", srcRootfs, err)
	}
	if err := os.MkdirAll(filepath.Dir(dstDisk), 0o755); err != nil {
		return fmt.Errorf("create disk dir: %w", err)
	}
	tmpDisk := dstDisk + ".tmp"
	_ = os.Remove(tmpDisk)
	cmd := exec.Command("cp", "--reflink=auto", "--sparse=always", srcRootfs, tmpDisk)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmpDisk)
		return fmt.Errorf("clone disk: %w: %s", err, strings.TrimSpace(string(out)))
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
		return fmt.Errorf("marshal bootstrap config: %w", err)
	}
	data = append(data, '\n')

	tmpFile, err := os.CreateTemp("", "lisavm-bootstrap-*.json")
	if err != nil {
		return fmt.Errorf("create temp bootstrap file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp bootstrap file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp bootstrap file: %w", err)
	}

	_ = exec.Command("debugfs", "-w", "-R", "mkdir /etc/lisavm", diskPath).Run()
	_ = exec.Command("debugfs", "-w", "-R", "rm /etc/lisavm/bootstrap.json", diskPath).Run()

	writeCmd := exec.Command("debugfs", "-w", "-R", fmt.Sprintf("write %s /etc/lisavm/bootstrap.json", tmpPath), diskPath)
	if out, err := writeCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("debugfs write bootstrap.json: %w: %s", err, strings.TrimSpace(string(out)))
	}

	statCmd := exec.Command("debugfs", "-R", "stat /etc/lisavm/bootstrap.json", diskPath)
	if out, err := statCmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "Inode:") {
		return fmt.Errorf("verify bootstrap.json on %s failed: %v %s", diskPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *LinuxProvisioner) EnsureTAP(tapName, bridgeName string) error {
	if err := exec.Command("ip", "link", "show", tapName).Run(); err != nil {
		cmd := exec.Command("ip", "tuntap", "add", "dev", tapName, "mode", "tap")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("create tap %s: %w: %s", tapName, err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := exec.Command("ip", "link", "set", tapName, "master", bridgeName).CombinedOutput(); err != nil {
		return fmt.Errorf("attach tap %s to %s: %w: %s", tapName, bridgeName, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("ip", "link", "set", tapName, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("bring up tap %s: %w: %s", tapName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *LinuxProvisioner) DeleteTAP(tapName string) error {
	if tapName == "" {
		return nil
	}
	if err := exec.Command("ip", "link", "show", tapName).Run(); err != nil {
		return nil
	}
	if out, err := exec.Command("ip", "link", "del", tapName).CombinedOutput(); err != nil {
		return fmt.Errorf("delete tap %s: %w: %s", tapName, err, strings.TrimSpace(string(out)))
	}
	return nil
}
