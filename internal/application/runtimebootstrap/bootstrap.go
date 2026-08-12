package runtimebootstrap

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/yy003x/runtime/internal/application/toolruntime"
	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/envref"
	"github.com/yy003x/runtime/internal/infrastructure/executionlog"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/internal/infrastructure/runtimeconfig"
	"github.com/yy003x/runtime/internal/infrastructure/toolbuiltin"
	"github.com/yy003x/runtime/internal/infrastructure/toolconfig"
	"github.com/yy003x/runtime/internal/infrastructure/toolmcp"
	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/model"
	"github.com/yy003x/runtime/pkg/profile"
	"github.com/yy003x/runtime/pkg/provider/anthropic"
	"github.com/yy003x/runtime/pkg/provider/openai"
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
	sqlitestore "github.com/yy003x/runtime/pkg/store/sqlite"
	runtimetmux "github.com/yy003x/runtime/pkg/tmux"
)

type ProfileServices struct {
	Profiles *profile.Catalog
	Models   model.Generator
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
			model.DriverOpenAI:    openai.New(nil),
			model.DriverAnthropic: anthropic.New(nil),
		},
		model.ServiceOptions{AttemptObserver: executionAttemptObserver(paths.LogsDir)},
	)
	if err != nil {
		return nil, fmt.Errorf("build model service: %w", err)
	}
	resilient := model.NewResilientModel(models, model.DefaultRetryPolicy())
	return &ProfileServices{Profiles: profiles, Models: resilient}, nil
}

func executionAttemptObserver(logsDir string) model.AttemptObserver {
	return func(attempt model.Attempt) {
		callID, err := identity.New("call")
		if err != nil {
			return
		}
		_ = executionlog.AppendAPI(logsDir, executionlog.APIRecord{
			Time:      time.Now(),
			Namespace: executionlog.Namespace(attempt.Origin.Namespace),
			Profile:   attempt.ProfileID,
			Source:    attempt.Origin.Source,
			CallID:    callID,
			Request:   attempt.Wire.Request,
			Response:  attempt.Wire.Response,
			Error:     attempt.Error,
		})
	}
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
	Sessions                  *session.Service
	Runs                      *runtime.Service
	Tools                     agent.ToolExecutor
	ToolEnvironmentReferences []string
}

type SessionServices struct {
	*ProfileServices
	Sessions *session.Service
}

type SessionRunServices struct {
	Sessions *session.Service
	Runs     *runtime.Service
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

// LoadTmuxService validates canonical runtime.json, consumes only the narrow
// tmux.server_mode setting, and otherwise composes the independent Tmux domain
// without Profile, Session, or SQLite state.
func LoadTmuxService(paths layout.Paths) (*runtimetmux.Service, error) {
	config, err := runtimeconfig.Load(paths.RuntimeConfigFile)
	if err != nil {
		return nil, fmt.Errorf("load runtime config for Tmux: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve sn-cli helper executable: %w", err)
	}
	service, err := runtimetmux.NewService(runtimetmux.Config{
		Home: paths.Home, ServerMode: runtimetmux.ServerMode(config.Tmux.ServerMode),
		LockFile:       paths.TmuxLockFile,
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

// LoadSessionRunServices composes the narrow durable Session execution path.
// It deliberately excludes Agent tools and Agent executors while retaining the
// same SQLite Run and file-backed Session facts used by the full Runtime.
func LoadSessionRunServices(
	paths layout.Paths,
	reservedProfileIDs ...string,
) (*SessionRunServices, error) {
	core, err := LoadProfileServices(paths, reservedProfileIDs...)
	if err != nil {
		return nil, err
	}
	sessions, err := loadSessionExecutionService(paths, core)
	if err != nil {
		return nil, err
	}
	runStore, err := sqlitestore.Open(
		paths.RunDBFile,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		return nil, fmt.Errorf("open Run store: %w", err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: runStore,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindSession: &runtime.SessionExecutor{
				Profiles: core.Profiles, Sessions: sessions,
			},
		},
	})
	if err != nil {
		_ = runStore.Close()
		return nil, fmt.Errorf("build Session Run service: %w", err)
	}
	return &SessionRunServices{Sessions: sessions, Runs: runs}, nil
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
		LogsDir: paths.LogsDir,
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
	tools, toolEnvironmentReferences, err := buildAgentTools(
		paths.ToolsDir, cwd, core.Config.Agent, core.Config.MCP,
	)
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
		ToolEnvironmentReferences: toolEnvironmentReferences,
	}, nil
}

func buildAgentTools(
	toolsDirectory string,
	cwd string,
	configuration runtimeconfig.Agent,
	mcp runtimeconfig.MCP,
) (*agent.Registry, []string, error) {
	builtinNames := make([]string, 0, len(configuration.Tools))
	manifestNames := make([]string, 0, len(configuration.Tools))
	for _, name := range configuration.Tools {
		if toolbuiltin.IsBuiltin(name) {
			builtinNames = append(builtinNames, name)
			continue
		}
		manifestNames = append(manifestNames, name)
	}
	components := make([]toolruntime.Component, 0, 2)
	if len(builtinNames) > 0 {
		bundle, err := toolbuiltin.BuildBundle(toolbuiltin.Options{
			Names: builtinNames, Roots: configuration.WorkspaceRoots, CWD: cwd,
		})
		if err != nil {
			return nil, nil, err
		}
		components = append(components, toolruntime.Component{
			Identity: agent.ToolExecutionIdentity{
				Implementation:        toolbuiltin.ExecutionImplementation,
				ImplementationVersion: toolbuiltin.ExecutionImplementationVersion,
				Configuration:         bundle.Configuration,
			},
			Tools: bundle.Tools,
		})
	}
	var references []string
	catalogRequired := len(manifestNames) > 0
	if _, err := os.Lstat(toolsDirectory); err == nil || !os.IsNotExist(err) {
		catalogRequired = true
	}
	if catalogRequired {
		catalog, err := toolconfig.LoadDirectory(toolsDirectory)
		if err != nil {
			return nil, nil, fmt.Errorf("load Tool Catalog: %w", err)
		}
		for _, name := range catalog.Names() {
			if toolbuiltin.IsBuiltin(name) {
				return nil, nil, fmt.Errorf(
					"Tool Catalog name %q conflicts with a built-in tool", name,
				)
			}
		}
		if len(manifestNames) > 0 {
			manifests, err := catalog.Select(manifestNames)
			if err != nil {
				return nil, nil, fmt.Errorf("select Tool Catalog entries: %w", err)
			}
			bundle, err := toolmcp.Build(manifests, toolmcp.Options{
				RequestedProtocolVersion: mcp.RequestedProtocolVersion,
				AllowedProtocolVersions:  mcp.AllowedProtocolVersions,
			})
			if err != nil {
				return nil, nil, fmt.Errorf("build MCP tools: %w", err)
			}
			components = append(components, toolruntime.Component{
				Identity: agent.ToolExecutionIdentity{
					Implementation:        toolmcp.ExecutionImplementation,
					ImplementationVersion: toolmcp.ExecutionImplementationVersion,
					Configuration:         bundle.Configuration,
				},
				Tools: bundle.Tools,
			})
			references = manifestEnvironmentReferences(manifests)
		}
	}
	registry, err := toolruntime.Build(components...)
	if err != nil {
		return nil, nil, err
	}
	return registry, references, nil
}

func manifestEnvironmentReferences(
	manifests []toolconfig.Manifest,
) []string {
	seen := make(map[string]struct{})
	for _, manifest := range manifests {
		for _, value := range manifest.Executor.Headers {
			for _, name := range envref.References(value) {
				seen[name] = struct{}{}
			}
		}
	}
	values := make([]string, 0, len(seen))
	for name := range seen {
		values = append(values, name)
	}
	sort.Strings(values)
	return values
}
