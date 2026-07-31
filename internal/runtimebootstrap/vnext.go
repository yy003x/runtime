package runtimebootstrap

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimeconfig"
	"github.com/yy003x/runtime/internal/toolbuiltin"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/profile"
	"github.com/yy003x/runtime/provider/anthropic"
	"github.com/yy003x/runtime/provider/openai"
	runtime "github.com/yy003x/runtime/run"
	"github.com/yy003x/runtime/session"
	sqlitestore "github.com/yy003x/runtime/store/sqlite"
	runtimetmux "github.com/yy003x/runtime/tmux"
)

type ProfileServices struct {
	Profiles *profile.Catalog
	Models   *model.Service
}

type VNext struct {
	*ProfileServices
	Config runtimeconfig.Config
}

func LoadProfileServices(
	paths layout.Paths,
	reservedProfileIDs ...string,
) (*ProfileServices, error) {
	profiles, err := profile.Load(paths.ConfigDir, reservedProfileIDs...)
	if err != nil {
		return nil, err
	}
	models, err := model.NewService(
		profiles.ModelCatalog(),
		map[model.DriverName]model.Driver{
			model.DriverOpenAICompatible:    openai.New(nil),
			model.DriverAnthropicCompatible: anthropic.New(nil),
		},
		model.ServiceOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("build model service: %w", err)
	}
	return &ProfileServices{Profiles: profiles, Models: models}, nil
}

func LoadVNext(paths layout.Paths, reservedProfileIDs ...string) (*VNext, error) {
	profiles, err := LoadProfileServices(paths, reservedProfileIDs...)
	if err != nil {
		return nil, err
	}
	config, err := runtimeconfig.Load(paths.RuntimeConfigFile)
	if err != nil {
		return nil, fmt.Errorf("load runtime config: %w", err)
	}
	return &VNext{ProfileServices: profiles, Config: config}, nil
}

type Services struct {
	*VNext
	Sessions *session.Service
	Runs     *runtime.Service
	Tools    agent.ToolExecutor
}

type SessionServices struct {
	*ProfileServices
	Sessions *session.Service
}

type SessionMaintenanceServices struct {
	Sessions *session.Service
}

type RunQueryServices struct {
	Runs *runtime.QueryService
}

type RunMaintenanceServices struct {
	Sessions *session.Service
	Runs     *runtime.Service
}

// LoadTmuxService composes the independent Tmux domain from layout only. It
// deliberately does not load Profile, Session, runtime.json, or SQLite state.
func LoadTmuxService(paths layout.Paths) (*runtimetmux.Service, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve sn-cli helper executable: %w", err)
	}
	service, err := runtimetmux.NewService(runtimetmux.Config{
		Home: paths.Home, LockFile: paths.TmuxLockFile,
		ManifestDir:    paths.TmuxManifestDir,
		TmuxConfigFile: paths.TmuxConfigFile,
		SocketDir:      paths.TmuxSocketDir, SocketFile: paths.TmuxSocketFile,
		HelperCommand: []string{
			executable, runtimetmux.HelperCommandName,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build Tmux service: %w", err)
	}
	return service, nil
}

func LoadSessionServices(
	paths layout.Paths,
	reservedProfileIDs ...string,
) (*SessionServices, error) {
	core, err := LoadProfileServices(paths, reservedProfileIDs...)
	if err != nil {
		return nil, err
	}
	sessions, err := loadSessionExecutionService(paths, core)
	if err != nil {
		return nil, err
	}
	return &SessionServices{ProfileServices: core, Sessions: sessions}, nil
}

func LoadSessionMaintenanceServices(
	paths layout.Paths,
) (*SessionMaintenanceServices, error) {
	sessionStore, err := session.NewStore(
		paths.SessionsDir, paths.StateDir,
	)
	if err != nil {
		return nil, fmt.Errorf("build Session store: %w", err)
	}
	sessions, err := session.NewMaintenanceService(sessionStore)
	if err != nil {
		return nil, fmt.Errorf("build Session maintenance service: %w", err)
	}
	return &SessionMaintenanceServices{Sessions: sessions}, nil
}

// LoadRunQueryServices composes only the SQLite-backed Run query/GC surface.
// It deliberately does not load Profile, Provider, tools, Session, or
// runtime.json and never performs startup reconciliation.
func LoadRunQueryServices(paths layout.Paths) (*RunQueryServices, error) {
	runStore, err := sqlitestore.Open(
		paths.RunDBFile,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		return nil, fmt.Errorf("open Run store: %w", err)
	}
	runs, err := runtime.NewQueryService(runStore)
	if err != nil {
		_ = runStore.Close()
		return nil, fmt.Errorf("build Run query service: %w", err)
	}
	return &RunQueryServices{Runs: runs}, nil
}

// LoadRunSettledRetention reads only the Run GC default from runtime.json.
func LoadRunSettledRetention(paths layout.Paths) (time.Duration, error) {
	config, err := runtimeconfig.Load(paths.RuntimeConfigFile)
	if err != nil {
		return 0, fmt.Errorf("load runtime config: %w", err)
	}
	return config.SettledRetention(), nil
}

// LoadRunMaintenanceServices composes explicit cancellation/reconciliation
// without execution dependencies. Agent and Session executors receive only
// the Stores needed by their maintenance methods.
func LoadRunMaintenanceServices(
	paths layout.Paths,
) (*RunMaintenanceServices, error) {
	runStore, err := sqlitestore.Open(
		paths.RunDBFile,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		return nil, fmt.Errorf("open Run store: %w", err)
	}
	sessionStore, err := session.NewStore(
		paths.SessionsDir, paths.StateDir,
	)
	if err != nil {
		_ = runStore.Close()
		return nil, fmt.Errorf("build Session store: %w", err)
	}
	sessions, err := session.NewMaintenanceService(sessionStore)
	if err != nil {
		_ = runStore.Close()
		return nil, fmt.Errorf("build Session maintenance service: %w", err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: runStore,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Store: runStore, Sessions: sessions,
			},
			runtime.KindSession: &runtime.SessionExecutor{
				Sessions: sessions,
			},
		},
	})
	if err != nil {
		_ = runStore.Close()
		return nil, fmt.Errorf("build Run maintenance service: %w", err)
	}
	return &RunMaintenanceServices{
		Sessions: sessions, Runs: runs,
	}, nil
}

func loadSessionExecutionService(
	paths layout.Paths,
	core *ProfileServices,
) (*session.Service, error) {
	sessionStore, err := session.NewStore(
		paths.SessionsDir, paths.StateDir,
	)
	if err != nil {
		return nil, fmt.Errorf("build Session store: %w", err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: core.Profiles, Models: core.Models,
	})
	if err != nil {
		return nil, fmt.Errorf("build Session service: %w", err)
	}
	return sessions, nil
}

func LoadServices(
	paths layout.Paths,
	cwd string,
	reservedProfileIDs ...string,
) (*Services, error) {
	return loadServices(paths, cwd, true, reservedProfileIDs...)
}

func LoadServicesWithRunRecovery(
	paths layout.Paths,
	cwd string,
	reservedProfileIDs ...string,
) (*Services, error) {
	return loadServices(paths, cwd, false, reservedProfileIDs...)
}

func loadServices(
	paths layout.Paths,
	cwd string,
	skipRunRecovery bool,
	reservedProfileIDs ...string,
) (*Services, error) {
	core, err := LoadVNext(paths, reservedProfileIDs...)
	if err != nil {
		return nil, err
	}
	sessions, err := loadSessionExecutionService(paths, core.ProfileServices)
	if err != nil {
		return nil, err
	}
	tools, err := toolbuiltin.Build(toolbuiltin.Options{
		Names: core.Config.Agent.Tools,
		Roots: core.Config.Agent.WorkspaceRoots,
		CWD:   cwd,
	})
	if err != nil {
		return nil, fmt.Errorf("build Agent tools: %w", err)
	}
	runStore, err := sqlitestore.Open(
		paths.RunDBFile,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		return nil, fmt.Errorf("open Run store: %w", err)
	}
	agentExecutor := &runtime.AgentExecutor{
		Profiles: core.Profiles, Model: core.Models, Tools: tools,
		Store: runStore, Sessions: sessions,
	}
	sessionExecutor := &runtime.SessionExecutor{
		Profiles: core.Profiles, Sessions: sessions,
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: runStore,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: agentExecutor, runtime.KindSession: sessionExecutor,
		},
	})
	if err != nil {
		runStore.Close()
		return nil, fmt.Errorf("build Run service: %w", err)
	}
	if !skipRunRecovery {
		if err := runs.Reconcile(context.Background()); err != nil {
			_ = runs.Close()
			return nil, fmt.Errorf("reconcile Run service: %w", err)
		}
	}
	return &Services{
		VNext: core, Sessions: sessions, Runs: runs, Tools: tools,
	}, nil
}
