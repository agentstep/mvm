package firecracker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentstep/mvm/internal/agentclient"
	"github.com/agentstep/mvm/internal/state"
)

// Seccomp profiles — restrict syscalls inside the guest.
var seccompProfiles = map[string]string{
	"strict": `iptables -A OUTPUT -p tcp --dport 80 -j DROP
iptables -A OUTPUT -p tcp --dport 443 -j DROP
chmod 000 /usr/bin/wget /usr/bin/curl 2>/dev/null || true
chmod 000 /sbin/apk 2>/dev/null || true
mount -o remount,ro /`,

	"moderate": `chmod 000 /sbin/apk 2>/dev/null || true
echo "Moderate seccomp profile applied"`,

	"permissive": `echo "Permissive seccomp profile — no restrictions, audit only"`,
}

// maxVolumeCopyBytes caps the raw (pre-base64) size of a single volume's tar
// archive. The agent protocol's frame size is capped at 10 MiB
// (agent/internal/protocol/frame.go, internal/agentclient/protocol.go's
// maxFrameSize) and the whole request — command string + base64 stdin — is
// one frame. Base64 inflates by ~4/3; 6 MiB raw -> ~8 MiB encoded, leaving
// headroom for the JSON envelope under the 10 MiB cap.
//
// This is the concrete shape of Firecracker's copy-in trade-off: fine for
// source trees and config, not a general bind-mount replacement. Anything
// bigger needs applevz's live virtiofs share instead.
const maxVolumeCopyBytes = 6 * 1024 * 1024

// SetupVolumeMounts copies each host directory into the guest at boot via a
// one-shot tar transfer over the guest agent's vsock connection — NOT a live
// mount. Firecracker's VMM has never implemented virtio-fs or 9p, so there is
// no live host<->guest filesystem sharing available on this backend; changes
// made on either side after this call are never synced. See the "applevz"
// backend (internal/vm/applevz.go, internal/cli/start.go's runStartAppleVZ)
// for the live-mount path.
//
// Every volume's format is validated up front, before any network I/O, so a
// single malformed -V entry fails fast without partially copying others.
func SetupVolumeMounts(vm *state.VM, volumes []string) error {
	type parsedVolume struct{ hostPath, guestPath string }

	parsed := make([]parsedVolume, 0, len(volumes))
	for _, vol := range volumes {
		parts := strings.SplitN(vol, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid volume format %q (expected hostPath:guestPath)", vol)
		}
		parsed = append(parsed, parsedVolume{hostPath: parts[0], guestPath: parts[1]})
	}

	for _, v := range parsed {
		if err := copyDirIntoGuest(vm.Name, v.hostPath, v.guestPath); err != nil {
			return fmt.Errorf("volume %s:%s: %w", v.hostPath, v.guestPath, err)
		}
	}
	return nil
}

// copyDirIntoGuest tars hostPath, base64-encodes it, and ships it to the
// guest agent's non-interactive Exec (internal/agentclient/client.go's
// Client.Exec) with a command that decodes and extracts it under guestPath.
// Exec's stdin is a plain JSON string field (agent/internal/protocol/frame.go),
// not a raw byte channel, so base64 is required here — sending raw tar bytes
// through a JSON string would corrupt any non-UTF-8 byte.
func copyDirIntoGuest(vmName, hostPath, guestPath string) error {
	data, err := buildTarArchive(hostPath)
	if err != nil {
		return err
	}

	client := agentclient.New(&agentclient.FirecrackerVsockDialer{UDSPath: VsockUDSPath(vmName)})
	cmd := fmt.Sprintf("mkdir -p %s && base64 -d | tar -xf - -C %s",
		shellQuoteForSSH(guestPath), shellQuoteForSSH(guestPath))

	// No explicit deadline: Client.exchange applies agentclient.DefaultRequestTimeout
	// (5 minutes) when ctx has none — plenty for a <=6MiB transfer.
	result, err := client.Exec(context.Background(), cmd, base64.StdEncoding.EncodeToString(data))
	if err != nil {
		return fmt.Errorf("copy into guest: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("tar extract exited %d: %s", result.ExitCode, result.Output)
	}
	return nil
}

// buildTarArchive tars the contents of hostDir (paths relative to hostDir,
// suitable for extraction with `tar -C <dest>`), enforcing maxVolumeCopyBytes
// before the archive is ever sent anywhere.
func buildTarArchive(hostDir string) ([]byte, error) {
	info, err := os.Stat(hostDir)
	if err != nil {
		return nil, fmt.Errorf("stat host path: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("host path %q is not a directory", hostDir)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err = filepath.WalkDir(hostDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == hostDir {
			return nil
		}
		rel, err := filepath.Rel(hostDir, path)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
		if buf.Len() > maxVolumeCopyBytes {
			return fmt.Errorf("host directory %q exceeds the %d-byte Firecracker copy-in limit (use applevz for larger shares)", hostDir, maxVolumeCopyBytes)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > maxVolumeCopyBytes {
		return nil, fmt.Errorf("host directory %q exceeds the %d-byte Firecracker copy-in limit (use applevz for larger shares)", hostDir, maxVolumeCopyBytes)
	}
	return buf.Bytes(), nil
}

func shellQuoteForSSH(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
