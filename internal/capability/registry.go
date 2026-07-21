package capability

// Registry is the single loader for Runtime skills, tools and memory stores.
// CLI, API, native and loop callers pass their resolved active paths here
// instead of each reimplementing directory registration.
type Registry struct {
	Skills *SkillManager
	Tools  *ToolManager
	config RegistryConfig
}

type RegistryConfig struct {
	SkillsDir            string
	ToolsDir             string
	MemoryFile           string
	MemoryCandidatesFile string
}

func NewRegistry(config RegistryConfig) *Registry {
	registry := &Registry{Skills: NewSkillManager(), Tools: NewToolManager(), config: config}
	if config.SkillsDir != "" {
		registry.Skills.RegisterDir(config.SkillsDir)
	}
	if config.ToolsDir != "" {
		registry.Tools.RegisterDir(config.ToolsDir)
	}
	return registry
}

func (r *Registry) Memory() (*Memory, error) {
	return OpenMemory(r.config.MemoryFile)
}

func (r *Registry) MemoryCandidates() (*Memory, error) {
	return OpenMemory(r.config.MemoryCandidatesFile)
}
