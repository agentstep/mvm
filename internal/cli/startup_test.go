package cli

import (
	"context"
	"strings"
	"testing"
)

// fakeRecipeAgent implements recipeAgent for tests — no vsock, no daemon,
// no real VM.
type fakeRecipeAgent struct {
	calls  []string
	execFn func(command string) (string, int, error)
}

func (f *fakeRecipeAgent) Exec(ctx context.Context, command, stdin string) (string, int, error) {
	f.calls = append(f.calls, command)
	if f.execFn != nil {
		return f.execFn(command)
	}
	return "", 0, nil
}

func TestRunStartupRecipeRunsCommandsInOrder(t *testing.T) {
	agent := &fakeRecipeAgent{}
	spec := &StartupSpec{
		Workdir: "/workspace",
		Commands: []StartupCommand{
			{Name: "install", Run: "npm install"},
			{Name: "build", Run: "npm run build"},
		},
	}
	if err := runStartupRecipe(context.Background(), agent, spec, newPhaseTimer(), func(string, ...any) {}); err != nil {
		t.Fatalf("runStartupRecipe: %v", err)
	}
	if len(agent.calls) != 3 { // mkdir workdir + 2 commands
		t.Fatalf("calls = %v, want 3 (mkdir + 2 commands)", agent.calls)
	}
	if !strings.Contains(agent.calls[1], "npm install") || !strings.Contains(agent.calls[2], "npm run build") {
		t.Errorf("calls = %v, want install then build in order", agent.calls)
	}
}

func TestRunStartupRecipeFailsFastOnNonZeroExit(t *testing.T) {
	agent := &fakeRecipeAgent{execFn: func(command string) (string, int, error) {
		if strings.Contains(command, "this-fails") {
			return "boom", 1, nil
		}
		return "", 0, nil
	}}
	spec := &StartupSpec{
		Commands: []StartupCommand{
			{Name: "bad", Run: "this-fails"},
			{Name: "unreached", Run: "echo never"},
		},
	}
	err := runStartupRecipe(context.Background(), agent, spec, newPhaseTimer(), func(string, ...any) {})
	if err == nil {
		t.Fatal("runStartupRecipe() = nil, want an error from the failing command")
	}
	if len(agent.calls) != 2 { // mkdir + the failing command; "unreached" must never run
		t.Errorf("calls = %v, want exactly 2 (mkdir + failing command)", agent.calls)
	}
}

func TestRecipeAgentAdaptersSatisfyInterface(t *testing.T) {
	var _ recipeAgent = applevzRecipeAgent{}
	var _ recipeAgent = daemonRecipeAgent{}
}
