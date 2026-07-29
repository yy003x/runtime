package runtimebootstrap

import (
	"fmt"
	"os"

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

type VNext struct {
	Profiles    *profile.Catalog
	Subcommands *profile.SubcommandCatalog
	Models      *model.Service
	Config      runtimeconfig.Config
}

func LoadVNext(paths layout.Paths, fixedCommandIDs ...string) (*VNext, error) {
	profiles, err := profile.Load(paths.ConfigDir)
	if err != nil {
		return nil, err
	}
	subcommands, err := profile.LoadSubcommands(
		paths.CommandDir, profiles, fixedCommandIDs...,
	)
	if err != nil {
		return nil, err
	}
	config, err := runtimeconfig.Load(paths.RuntimeConfigFile)
	if err != nil {
		return nil, fmt.Errorf("load runtime config: %w", err)
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
	return &VNext{
		Profiles: profiles, Subcommands: subcommands, Models: models, Config: config,
	}, nil
}

type Services struct {
	*VNext
	Sessions *session.Service
	Runs     *runtime.Service
	Tools    agent.ToolExecutor
}

type SessionServices struct {
	*VNext
	Sessions *session.Service
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
	fixedCommandIDs ...string,
) (*SessionServices, error) {
	core, err := LoadVNext(paths, fixedCommandIDs...)
	if err != nil {
		return nil, err
	}
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
	return &SessionServices{VNext: core, Sessions: sessions}, nil
}

func LoadServices(
	paths layout.Paths,
	cwd string,
	fixedCommandIDs ...string,
) (*Services, error) {
	sessionServices, err := LoadSessionServices(paths, fixedCommandIDs...)
	if err != nil {
		return nil, err
	}
	core := sessionServices.VNext
	sessions := sessionServices.Sessions
	tools, err := toolbuiltin.Build(toolbuiltin.Options{
		Names: core.Config.Agent.Tools,
		Roots: core.Config.Agent.WorkspaceRoots,
		CWD:   cwd,
	})
	if err != nil {
		return nil, fmt.Errorf("build Agent tools: %w", err)
	}
	runStore, err := sqlitestore.Open(paths.RunDBFile, sqlitestore.Options{})
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
	return &Services{
		VNext: core, Sessions: sessions, Runs: runs, Tools: tools,
	}, nil
}
