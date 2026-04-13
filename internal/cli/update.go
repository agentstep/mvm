package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

func newUpdateCmd(currentVersion string) *cobra.Command {
	var check bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update mvm to the latest version",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(currentVersion, check)
		},
	}

	cmd.Flags().BoolVar(&check, "check", false, "check for updates without installing")

	return cmd
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
	HTMLURL string    `json:"html_url"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func runUpdate(currentVersion string, checkOnly bool) error {
	fmt.Printf("Current version: %s\n", currentVersion)

	// Fetch latest release from GitHub
	resp, err := http.Get("https://api.github.com/repos/agentstep/mvm/releases/latest")
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		fmt.Println("No releases found. You're running from source.")
		return nil
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("failed to parse release info: %w", err)
	}

	latest := release.TagName
	if latest == currentVersion || latest == "v"+currentVersion {
		fmt.Printf("Already up to date (%s)\n", latest)
		return nil
	}

	fmt.Printf("Latest version: %s\n", latest)

	if checkOnly {
		fmt.Printf("Update available: %s → %s\n", currentVersion, latest)
		fmt.Printf("Run 'mvm update' to install.\n")
		return nil
	}

	// Find the right asset for this platform
	wantArch := runtime.GOARCH
	wantOS := runtime.GOOS
	var assetURL string
	for _, a := range release.Assets {
		name := strings.ToLower(a.Name)
		if strings.Contains(name, wantOS) && strings.Contains(name, wantArch) && strings.HasSuffix(name, ".tar.gz") {
			assetURL = a.BrowserDownloadURL
			break
		}
	}

	if assetURL == "" {
		fmt.Printf("No pre-built binary for %s/%s. Update from source:\n", wantOS, wantArch)
		fmt.Println("  git pull && make install")
		return nil
	}

	// Download to temp file
	fmt.Printf("Downloading %s...\n", latest)
	tmpFile, err := os.CreateTemp("", "mvm-update-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())

	dlResp, err := http.Get(assetURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer dlResp.Body.Close()

	if _, err := io.Copy(tmpFile, dlResp.Body); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	tmpFile.Close()

	// Extract and install
	tmpDir, err := os.MkdirTemp("", "mvm-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	if err := exec.Command("tar", "xzf", tmpFile.Name(), "-C", tmpDir).Run(); err != nil {
		return fmt.Errorf("extract failed: %w", err)
	}

	// Find the binary
	binPath, err := exec.LookPath("mvm")
	if err != nil {
		// Try GOPATH/bin
		binPath = os.Getenv("GOPATH") + "/bin/mvm"
	}

	extractedBin := tmpDir + "/mvm"
	if _, err := os.Stat(extractedBin); os.IsNotExist(err) {
		return fmt.Errorf("binary not found in release archive")
	}

	// Replace current binary
	if err := os.Rename(extractedBin, binPath); err != nil {
		// Try with sudo if permission denied
		if err := exec.Command("sudo", "mv", extractedBin, binPath).Run(); err != nil {
			return fmt.Errorf("install failed: %w — try: sudo mv %s %s", err, extractedBin, binPath)
		}
	}

	fmt.Printf("Updated to %s\n", latest)
	return nil
}
