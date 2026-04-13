package main

import (
	"os"
	"path/filepath"

	"github.com/agentstep/mvm/internal/menubar"
	"github.com/caseymrm/menuet"
)

func main() {
	home, _ := os.UserHomeDir()

	app := menubar.New(menubar.Config{
		MvmDir: filepath.Join(home, ".mvm"),
	})

	go app.StartPolling()

	menuet.App().Label = "com.mvm.menubar"
	menuet.App().Children = app.MenuItems
	menuet.App().SetMenuState(&menuet.MenuState{Title: "mvm"})
	menuet.App().RunApplication()
}
