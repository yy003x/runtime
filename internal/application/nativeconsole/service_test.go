package nativeconsole

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/pkg/tmux"
)

type absentAfterStopManager struct{}

func (absentAfterStopManager) Start(
	context.Context,
	tmux.StartRequest,
) (tmux.StartResult, error) {
	panic("not used")
}

func (absentAfterStopManager) List(context.Context) ([]tmux.Window, error) {
	return []tmux.Window{}, nil
}

func (absentAfterStopManager) Stop(
	context.Context,
	string,
) (tmux.ActionResult, error) {
	return tmux.ActionResult{}, errors.New("fixture window disappeared")
}

func TestSupervisorEnvironmentKeepsBootstrapInputsWithoutProviderSecrets(
	t *testing.T,
) {
	value := supervisorEnvironment("/runtime-home", []string{
		"PATH=/opt/homebrew/bin:/usr/bin:/bin",
		"HOME=/Users/fixture",
		"TMPDIR=/private/tmp",
		"TERM=xterm-256color",
		"PROVIDER_TOKEN=secret",
		layout.HomeEnv + "=/wrong-home",
	})
	want := []string{
		layout.HomeEnv + "=/runtime-home",
		"PATH=/opt/homebrew/bin:/usr/bin:/bin",
		"HOME=/Users/fixture",
		"TMPDIR=/private/tmp",
	}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("supervisor environment = %#v, want %#v", value, want)
	}
}

func TestStopBoundWindowAcceptsConcurrentAutoClose(t *testing.T) {
	service := &Service{tmux: absentAfterStopManager{}}
	accepted, err := service.stopBoundWindow(
		context.Background(),
		"session_11111111111111111111111111111111",
		"tmux-fixture",
	)
	if err != nil || !accepted {
		t.Fatalf("accepted=%t error=%v", accepted, err)
	}
}
