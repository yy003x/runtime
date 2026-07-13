package agent

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"agent-arch/internal/config"
	"agent-arch/internal/llm"
	"agent-arch/internal/llm/anthropic"
	"agent-arch/internal/llm/openai"
	"agent-arch/internal/memory"
	"agent-arch/internal/persona"
	"agent-arch/internal/token"
)

type ClientFactory interface {
	New(provider string, cfg config.ProviderConfig) (llm.Client, error)
}

type DefaultClientFactory struct{}

func (DefaultClientFactory) New(provider string, cfg config.ProviderConfig) (llm.Client, error) {
	httpClient := &http.Client{
		Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
	}

	switch provider {
	case "openai":
		return openai.NewClient(cfg.BaseURL, cfg.APIKey, httpClient), nil
	case "anthropic":
		return anthropic.NewClient(cfg.BaseURL, cfg.APIKey, httpClient), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}

type Service struct {
	cfg      config.Config
	loader   *persona.Loader
	factory  ClientFactory
	runtime  *AgentRuntime
	mu       sync.RWMutex
	sessions map[string]Session
}

func NewService(ctx context.Context, cfg config.Config, personaDir string) (*Service, error) {
	_ = ctx

	store := memory.NewInMemoryStore()
	manager := memory.NewManager(store, memory.StubRetriever{}, cfg.Memory, token.NewApproxCounter())

	return &Service{
		cfg:      cfg,
		loader:   persona.NewLoader(personaDir),
		factory:  DefaultClientFactory{},
		runtime:  NewAgentRuntime(manager, NewFileTraceLogger(cfg.Debug.LLMTraceDir), cfg.Memory.ResponseReservedTokens),
		sessions: make(map[string]Session),
	}, nil
}

func NewServiceWithDeps(cfg config.Config, loader *persona.Loader, factory ClientFactory, manager *memory.Manager) *Service {
	return &Service{
		cfg:      cfg,
		loader:   loader,
		factory:  factory,
		runtime:  NewAgentRuntime(manager, NewFileTraceLogger(cfg.Debug.LLMTraceDir), cfg.Memory.ResponseReservedTokens),
		sessions: make(map[string]Session),
	}
}

func (s *Service) CreateAgent(ctx context.Context, req CreateAgentRequest) (CreateAgentResponse, error) {
	personaID := req.PersonaID
	if personaID == "" {
		personaID = s.cfg.Agent.DefaultPersona
	}

	p, err := s.loader.Load(ctx, personaID)
	if err != nil {
		return CreateAgentResponse{}, fmt.Errorf("load persona: %w", err)
	}

	provider := req.Provider
	if provider == "" {
		provider = p.ModelPolicy.Provider
	}
	if provider == "" {
		provider = s.cfg.Agent.DefaultProvider
	}

	model := req.Model
	if model == "" {
		model = p.ModelPolicy.Model
	}
	if model == "" {
		if providerCfg, ok := s.cfg.Providers[provider]; ok && providerCfg.Model != "" {
			model = providerCfg.Model
		}
	}
	if model == "" {
		model = s.cfg.Agent.DefaultModel
	}

	if _, err := s.newClient(provider); err != nil {
		return CreateAgentResponse{}, err
	}

	sessionID := fmt.Sprintf("sess_%d", time.Now().UnixNano())
	session := Session{
		ID:       sessionID,
		Persona:  p,
		Provider: provider,
		Model:    model,
	}

	s.mu.Lock()
	s.sessions[sessionID] = session
	s.mu.Unlock()

	if err := s.runtime.memory.InitSession(ctx, sessionID); err != nil {
		return CreateAgentResponse{}, fmt.Errorf("init session memory: %w", err)
	}

	return CreateAgentResponse{
		SessionID: sessionID,
		PersonaID: p.ID,
		Provider:  provider,
		Model:     model,
	}, nil
}

func (s *Service) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	session, err := s.getSession(req.SessionID)
	if err != nil {
		return ChatResponse{}, err
	}

	client, err := s.newClient(session.Provider)
	if err != nil {
		return ChatResponse{}, err
	}

	result, err := s.runtime.ExecuteTurn(ctx, session, client, req.Message)
	if err != nil {
		return ChatResponse{}, err
	}

	return ChatResponse{
		SessionID: req.SessionID,
		Message:   result.Message,
	}, nil
}

func (s *Service) GetMemory(ctx context.Context, sessionID string) (*memory.SessionMemory, error) {
	return s.runtime.memory.Get(ctx, sessionID)
}

func (s *Service) newClient(provider string) (llm.Client, error) {
	providerCfg, ok := s.cfg.Providers[provider]
	if !ok {
		return nil, fmt.Errorf("provider %q not configured", provider)
	}
	if !providerCfg.Enabled {
		return nil, fmt.Errorf("provider %q disabled", provider)
	}
	if providerCfg.TimeoutSeconds == 0 {
		providerCfg.TimeoutSeconds = 30
	}
	return s.factory.New(provider, providerCfg)
}

func (s *Service) getSession(sessionID string) (Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.sessions[sessionID]
	if !ok {
		return Session{}, fmt.Errorf("session %q not found", sessionID)
	}
	return session, nil
}
