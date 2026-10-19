package ops

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/pkg/errors"
)

func installedInitrd(bootDir string) (string, error) {
	entries, err := filepath.Glob(filepath.Join(bootDir, "loader/entries/*.conf"))
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no boot entries in %s", bootDir)
	}
	// OSTree's default entry sorts last, as in coreos-installer's get_bls_info().
	contents, err := os.ReadFile(entries[len(entries)-1])
	if err != nil {
		return "", err
	}
	var initrd string
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "initrd" {
			initrd = fields[1]
		}
	}
	if !filepath.IsAbs(initrd) || initrd == "/" || slices.Contains(strings.Split(initrd, "/"), "..") {
		return "", fmt.Errorf("invalid initrd path %q in boot entry", initrd)
	}
	return filepath.Join(bootDir, strings.TrimPrefix(initrd, "/")), nil
}

func (o *ops) preserveInitrdNetwork(bootDir string, deploy func() error) error {
	initrd, err := installedInitrd(bootDir)
	if err != nil {
		return errors.Wrap(err, "failed to find installed initramfs")
	}
	// /tmp is shared with the host by the installer container.
	networkDir, err := os.MkdirTemp("/tmp", "assisted-installer-network-")
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(networkDir); cleanupErr != nil {
			o.log.WithError(cleanupErr).Warn("failed to remove temporary initramfs networking directory")
		}
	}()
	keyfilesDir := filepath.Join(networkDir, "keyfiles")
	out, err := o.ExecPrivilegeCommand(nil, coreosInstallerExecutable,
		"pxe", "network", "unwrap", "--directory", keyfilesDir, initrd)
	if err != nil {
		if strings.Contains(out, "No embedded network settings.") {
			// Older installers write /boot/coreos-firstboot-network, which
			// already survives OS replacement. Nothing needs transferring.
			return deploy()
		}
		return errors.Wrapf(err, "failed to extract initramfs networking: %s", out)
	}
	o.log.Info("Preserving embedded firstboot networking across OS image replacement")
	keyfiles, err := os.ReadDir(keyfilesDir)
	if err != nil {
		return err
	}
	archive := filepath.Join(networkDir, "network.img")
	args := []string{"pxe", "network", "wrap", "--output", archive}
	for _, keyfile := range keyfiles {
		args = append(args, "--keyfile", filepath.Join(keyfilesDir, keyfile.Name()))
	}
	if out, err = o.ExecPrivilegeCommand(nil, coreosInstallerExecutable, args...); err != nil {
		return errors.Wrapf(err, "failed to wrap initramfs networking: %s", out)
	}
	if err = deploy(); err != nil {
		return err
	}
	initrd, err = installedInitrd(bootDir)
	if err != nil {
		return errors.Wrap(err, "failed to find replacement initramfs")
	}
	return errors.Wrap(appendInitrdNetwork(initrd, archive), "failed to restore initramfs networking")
}

func appendInitrdNetwork(initrd, archive string) error {
	source, err := os.Open(initrd)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	network, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer network.Close()
	tmp, err := os.CreateTemp(filepath.Dir(initrd), ".assisted-installer-initrd-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, io.MultiReader(source, network)); err != nil {
		return err
	}
	owner := info.Sys().(*syscall.Stat_t)
	if err := tmp.Chown(int(owner.Uid), int(owner.Gid)); err != nil {
		return err
	}
	if err := tmp.Chmod(info.Mode()); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), initrd)
}
