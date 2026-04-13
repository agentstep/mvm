package menubar

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/caseymrm/menuet"
)

func (a *App) stopVM(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec.CommandContext(ctx, findMvm(), "stop", name).Run()
	a.poll()
}

func (a *App) deleteVM(name string) {
	if a.daemonRunning {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a.client.DeleteVM(ctx, name)
	} else {
		exec.Command(findMvm(), "delete", name, "--force").Run()
	}
	a.poll()
}

func (a *App) sshToVM(name string) {
	bin := findMvm()
	script := fmt.Sprintf(`tell application "Terminal" to do script "%s ssh %s"`, bin, name)
	exec.Command("osascript", "-e", script).Start()
}

func (a *App) openTerminal() {
	exec.Command("open", "-a", "Terminal").Start()
}

func showError(title string, err error) {
	menuet.App().Alert(menuet.Alert{
		MessageText:     title,
		InformativeText: err.Error(),
		Buttons:         []string{"OK"},
	})
}

func findMvm() string {
	self, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(self), "mvm")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if path, err := exec.LookPath("mvm"); err == nil {
		return path
	}
	return "mvm"
}
