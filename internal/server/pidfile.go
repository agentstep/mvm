package server

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func WritePID(path string) error {
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func ReadPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func IsRunning(path string) (bool, int, error) {
	pid, err := ReadPID(path)
	if err != nil || pid == 0 {
		return false, 0, err
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, pid, nil
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil, pid, nil
}

func RemovePID(path string) error {
	return os.Remove(path)
}

func CheckNotRunning(pidPath string) error {
	running, pid, _ := IsRunning(pidPath)
	if running {
		return fmt.Errorf("mvm server already running (PID %d). Stop it first: mvm serve stop", pid)
	}
	// Stale PID file — clean up
	if pid > 0 {
		os.Remove(pidPath)
	}
	return nil
}
