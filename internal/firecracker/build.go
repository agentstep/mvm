package firecracker

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

// BuildStep represents a single parsed Dockerfile directive.
type BuildStep struct {
	Directive string // "RUN", "ENV"
	Args      string
}

// ParseDockerfile reads a Dockerfile and extracts build steps that mvm supports.
// Supported directives: RUN (chroot exec), ENV (set in /etc/environment).
// FROM is validated but ignored. COPY emits a warning. Others are silently skipped.
func ParseDockerfile(path string) ([]BuildStep, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Dockerfile: %w", err)
	}
	defer f.Close()

	var steps []BuildStep
	var continuation strings.Builder
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := scanner.Text()

		// If we're accumulating a continuation, append this line.
		if continuation.Len() > 0 {
			trimmed := strings.TrimSpace(line)
			if strings.HasSuffix(trimmed, "\\") {
				continuation.WriteString(" ")
				continuation.WriteString(trimmed[:len(trimmed)-1])
				continue
			}
			continuation.WriteString(" ")
			continuation.WriteString(trimmed)
			line = continuation.String()
			continuation.Reset()
		} else {
			trimmed := strings.TrimSpace(line)

			// Skip empty lines and comments.
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}

			// Check for line continuation on a fresh line.
			if strings.HasSuffix(trimmed, "\\") {
				continuation.WriteString(trimmed[:len(trimmed)-1])
				continue
			}

			line = trimmed
		}

		// Parse directive and arguments.
		directive, args := splitDirective(line)
		if directive == "" {
			continue
		}

		switch directive {
		case "RUN":
			steps = append(steps, BuildStep{Directive: "RUN", Args: args})
		case "ENV":
			steps = append(steps, BuildStep{Directive: "ENV", Args: args})
		case "FROM":
			// Validate but ignore — mvm always uses its own base image.
			continue
		case "COPY":
			fmt.Fprintf(os.Stderr, "WARNING: COPY is not supported in mvm build, skipping\n")
			continue
		default:
			// WORKDIR, CMD, EXPOSE, etc. — silently skip.
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Dockerfile: %w", err)
	}

	return steps, nil
}

// splitDirective splits a Dockerfile line into its directive and arguments.
func splitDirective(line string) (string, string) {
	parts := strings.SplitN(line, " ", 2)
	if len(parts) == 0 {
		return "", ""
	}
	directive := strings.ToUpper(parts[0])
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return directive, args
}

// BuildRootfs creates a new rootfs image from the base image and applies build steps.
// It copies base.ext4, expands it, mounts it, and runs each step inside a chroot.
func BuildRootfs(exec Executor, cacheDir, imageName string, steps []BuildStep, sizeMB int) error {
	basePath := cacheDir + "/base.ext4"
	imagePath := cacheDir + "/" + imageName + ".ext4"
	mountPoint := cacheDir + "/build-mnt-" + imageName

	// Build the chroot commands for each step.
	var chrootCmds strings.Builder
	for _, step := range steps {
		switch step.Directive {
		case "RUN":
			escaped := strings.ReplaceAll(step.Args, "'", "'\\''")
			fmt.Fprintf(&chrootCmds, "chroot %s /bin/sh -c '%s'\n", mountPoint, escaped)
		case "ENV":
			// Parse KEY=VALUE or KEY VALUE format.
			key, value := parseEnvArg(step.Args)
			if key != "" {
				escaped := strings.ReplaceAll("export "+key+"="+value, "'", "'\\''")
				fmt.Fprintf(&chrootCmds, "echo '%s' >> %s/etc/environment\n", escaped, mountPoint)
			}
		}
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e

BASE_PATH='%s'
IMAGE_PATH='%s'
MNT='%s'

# Copy base image (sparse)
cp --sparse=always "$BASE_PATH" "$IMAGE_PATH"

# Expand image
truncate -s +%dM "$IMAGE_PATH"
e2fsck -fy "$IMAGE_PATH" || true
resize2fs "$IMAGE_PATH"

# Mount
mkdir -p "$MNT"
mount -o loop "$IMAGE_PATH" "$MNT"

# Cleanup trap
cleanup() {
    umount "$MNT/proc" 2>/dev/null || true
    umount "$MNT/sys" 2>/dev/null || true
    umount "$MNT/dev" 2>/dev/null || true
    umount "$MNT" 2>/dev/null || true
    rmdir "$MNT" 2>/dev/null || true
}
trap cleanup EXIT

# Bind-mount system directories
mount --bind /proc "$MNT/proc"
mount --bind /sys "$MNT/sys"
mount --bind /dev "$MNT/dev"

# Setup DNS
echo 'nameserver 8.8.8.8' > "$MNT/etc/resolv.conf"

# Execute build steps
%s
`, basePath, imagePath, mountPoint, sizeMB, chrootCmds.String())

	_, err := exec.RunWithTimeout(script, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("build rootfs %s: %w", imageName, err)
	}
	return nil
}

// parseEnvArg parses ENV arguments in Dockerfile format.
// Supports: KEY=VALUE and KEY VALUE forms.
func parseEnvArg(args string) (string, string) {
	args = strings.TrimSpace(args)
	if args == "" {
		return "", ""
	}

	// Try KEY=VALUE format first.
	if idx := strings.Index(args, "="); idx > 0 {
		key := args[:idx]
		value := args[idx+1:]
		return key, value
	}

	// KEY VALUE format.
	parts := strings.SplitN(args, " ", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}

	return parts[0], ""
}
