package ops

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openshift/assisted-installer/src/config"
	"github.com/openshift/assisted-installer/src/ops/execute"
	"github.com/sirupsen/logrus"
	"go.uber.org/mock/gomock"
)

func writeBootFile(t *testing.T, boot, name, contents string) {
	t.Helper()
	name = filepath.Join(boot, name)
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledInitrd(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		invalid     bool
	}{
		{name: "default deployment", entry: "linux /ostree/new/vmlinuz\ninitrd /ostree/new/initramfs.img\n"},
		{name: "whitespace", entry: " initrd\t/ostree/new/initramfs.img  \n"},
		{name: "missing initrd", entry: "linux /ostree/new/vmlinuz\n", invalid: true},
		{name: "relative path", entry: "initrd ostree/new/initramfs.img\n", invalid: true},
		{name: "path traversal", entry: "initrd /../etc/passwd\n", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boot := t.TempDir()
			writeBootFile(t, boot, "loader/entries/ostree-0-rhcos.conf", "initrd /ostree/old/initramfs.img\n")
			writeBootFile(t, boot, "loader/entries/ostree-1-rhcos.conf", tc.entry)
			got, err := installedInitrd(boot)
			if tc.invalid {
				if err == nil {
					t.Fatal("expected invalid boot entry to fail")
				}
			} else if want := filepath.Join(boot, "ostree/new/initramfs.img"); err != nil || got != want {
				t.Fatalf("got %q, want %q, error %v", got, want, err)
			}
		})
	}
	if _, err := installedInitrd(t.TempDir()); err == nil {
		t.Fatal("expected missing boot entries to fail")
	}
}

func TestPreserveInitrdNetwork(t *testing.T) {
	profile := "[connection]\nid=static\n[ipv4]\nmethod=manual\naddress1=192.0.2.10/24\n"
	for _, tc := range []struct {
		name                                  string
		embedded, legacy, broken, deployError bool
	}{
		{name: "embedded networking", embedded: true},
		{name: "older installer", legacy: true},
		{name: "no networking"},
		{name: "invalid initramfs", broken: true},
		{name: "failed deployment", embedded: true, deployError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boot := t.TempDir()
			writeBootFile(t, boot, "ostree/old/initramfs.img", "OLD KERNEL\n")
			writeBootFile(t, boot, "loader/entries/ostree-1-rhcos.conf", "initrd /ostree/old/initramfs.img\n")
			if tc.legacy {
				writeBootFile(t, boot, "coreos-firstboot-network/static.nmconnection", profile)
			}
			executor := execute.NewMockExecute(gomock.NewController(t))
			o := NewOpsWithConfig(&config.Config{}, logrus.New(), executor).(*ops)
			var networkDir string
			// Stub archive handling only; boot-entry parsing and file I/O are real.
			executor.EXPECT().ExecCommandWithOptions(nil, coreosInstallerExecutable, gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ io.Writer, _ string, args []string, _ ...execute.CommandOption) (string, error) {
					if len(args) != 6 || strings.Join(args[:4], " ") != "pxe network unwrap --directory" ||
						args[5] != filepath.Join(boot, "ostree/old/initramfs.img") {
						t.Fatalf("unexpected unwrap arguments: %v", args)
					}
					networkDir = filepath.Dir(args[4])
					if tc.broken {
						return "Error: invalid initramfs", errors.New("invalid initramfs")
					}
					if !tc.embedded {
						return "Error: No embedded network settings.", errors.New("exit status 1")
					}
					writeBootFile(t, args[4], "static connection.nmconnection", profile)
					return "", nil
				})
			if tc.embedded {
				executor.EXPECT().ExecCommandWithOptions(nil, coreosInstallerExecutable, gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ io.Writer, _ string, args []string, _ ...execute.CommandOption) (string, error) {
						if len(args) != 7 || strings.Join(args[:4], " ") != "pxe network wrap --output" || args[5] != "--keyfile" {
							t.Fatalf("unexpected wrap arguments: %v", args)
						}
						contents, err := os.ReadFile(args[6])
						if err != nil {
							t.Fatal(err)
						}
						// #nosec G703 -- The mock output is in preserveInitrdNetwork's temporary directory.
						return "", os.WriteFile(args[4], append([]byte("NETWORK\n"), contents...), 0o600)
					})
			}
			deployed := false
			err := o.preserveInitrdNetwork(boot, func() error {
				deployed = true
				if tc.deployError {
					return errors.New("deployment failed")
				}
				if err := os.Remove(filepath.Join(boot, "ostree/old/initramfs.img")); err != nil {
					t.Fatal(err)
				}
				writeBootFile(t, boot, "ostree/new/initramfs.img", "NEW KERNEL\n")
				writeBootFile(t, boot, "loader/entries/ostree-1-rhcos.conf", "initrd /ostree/new/initramfs.img\n")
				return nil
			})
			if _, statErr := os.Stat(networkDir); !os.IsNotExist(statErr) {
				t.Fatalf("temporary networking directory was not removed: %v", statErr)
			}
			if tc.broken || tc.deployError {
				wantDeployed := !tc.broken
				if err == nil || deployed != wantDeployed {
					t.Fatalf("expected failure, got deployed=%t, error %v", deployed, err)
				}
				return
			}
			if err != nil || !deployed {
				t.Fatalf("expected deployment to succeed, got deployed=%t, error %v", deployed, err)
			}
			want := "NEW KERNEL\n"
			if tc.embedded {
				want += "NETWORK\n" + profile
			}
			got, err := os.ReadFile(filepath.Join(boot, "ostree/new/initramfs.img"))
			if err != nil || string(got) != want {
				t.Fatalf("replacement initramfs: got %q, want %q, error %v", got, want, err)
			}
			if tc.legacy {
				got, err := os.ReadFile(filepath.Join(boot, "coreos-firstboot-network/static.nmconnection"))
				if err != nil || string(got) != profile {
					t.Fatalf("legacy networking changed: %q, error %v", got, err)
				}
			}
		})
	}
}

func TestAppendInitrdNetwork(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "append archive", true: "failed copy"}[broken], func(t *testing.T) {
			dir := t.TempDir()
			initrd, archive := filepath.Join(dir, "initramfs.img"), filepath.Join(dir, "network.img")
			// #nosec G306 -- Exercise preservation of the existing initramfs permissions.
			if err := os.WriteFile(initrd, []byte("KERNEL\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			if broken {
				if err := os.Mkdir(archive, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(archive, []byte("NETWORK\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := appendInitrdNetwork(initrd, archive)
			if (err != nil) != broken {
				t.Fatalf("unexpected append result: %v", err)
			}
			want := "KERNEL\nNETWORK\n"
			if broken {
				want = "KERNEL\n"
			}
			got, err := os.ReadFile(initrd)
			if err != nil || string(got) != want {
				t.Fatalf("initramfs: got %q, want %q, error %v", got, want, err)
			}
			info, err := os.Stat(initrd)
			if err != nil || info.Mode().Perm() != 0o640 {
				t.Fatalf("initramfs permissions changed, error %v", err)
			}
			leftovers, err := filepath.Glob(filepath.Join(dir, ".assisted-installer-initrd-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("temporary initramfs files remain: %v, error %v", leftovers, err)
			}
		})
	}
}
