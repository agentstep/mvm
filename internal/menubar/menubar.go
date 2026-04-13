package menubar

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/caseymrm/menuet"
)

type Config struct {
	MvmDir       string
	PollInterval time.Duration
}

type App struct {
	cfg    Config
	store  *state.Store
	client *server.Client

	mu            sync.RWMutex
	daemonRunning bool
	vms           []server.VMResponse
	poolReady     int
	poolTotal     int
}

func New(cfg Config) *App {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	return &App{
		cfg:    cfg,
		store:  state.NewStore(filepath.Join(cfg.MvmDir, "state.json")),
		client: server.NewClient(server.DefaultSocketPath()),
	}
}

func (a *App) StartPolling() {
	a.poll()
	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()
	for range ticker.C {
		a.poll()
	}
}

func (a *App) poll() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.daemonRunning = a.client.IsAvailable()

	if a.daemonRunning {
		vms, err := a.client.ListVMs(context.Background())
		if err == nil {
			a.vms = vms
		}
		ps, err := a.client.PoolStatus(context.Background())
		if err == nil {
			a.poolReady = ps.Ready
			a.poolTotal = ps.Total
		}
	} else {
		st, err := a.store.Load()
		if err == nil {
			a.vms = stateToResponses(st)
		} else {
			a.vms = nil
		}
		a.poolReady = 0
		a.poolTotal = 0
	}

	a.updateTitle()
	menuet.App().MenuChanged()
}

func (a *App) updateTitle() {
	if !a.daemonRunning {
		menuet.App().SetMenuState(&menuet.MenuState{Title: "mvm: off"})
		return
	}

	running := 0
	for _, vm := range a.vms {
		if vm.Status == "running" {
			running++
		}
	}

	title := fmt.Sprintf("mvm: %d", running)
	if a.poolTotal > 0 {
		title += fmt.Sprintf(" | pool %d/%d", a.poolReady, a.poolTotal)
	}

	menuet.App().SetMenuState(&menuet.MenuState{Title: title})
}

func stateToResponses(st *state.State) []server.VMResponse {
	result := make([]server.VMResponse, 0, len(st.VMs))
	for _, vm := range st.VMs {
		result = append(result, server.VMResponse{
			Name:      vm.Name,
			Status:    vm.Status,
			GuestIP:   vm.GuestIP,
			PID:       vm.PID,
			Backend:   vm.Backend,
			Ports:     vm.Ports,
			CreatedAt: vm.CreatedAt,
		})
	}
	return result
}
