package inbox

import (
	"fmt"

	"github.com/im-tyler/agent-inbox/internal/config"
	"github.com/im-tyler/agent-inbox/internal/ident"
)

// requireCurrentDefinitions refuses a write or a send from a frontend whose
// cached projects no longer match the on-disk configuration.
//
// The claim serializes ownership by name; it does not make the owner's
// cached configuration correct. A long-lived frontend that loaded a project
// as (tool, dir) can outlive another frontend's tool or directory change,
// and its next save would revert the newer entry while its sends drove the
// old identity against the user's current configuration. The configuration
// is the shared definition of the fleet, so it — not whichever process has
// the oldest memory — decides what a project is.
//
// This is deliberately a stop-and-reload policy rather than live
// configuration replacement: refusing is honest and small; silently
// migrating a session between tools or directories is neither.
//
// Call with no in.mu held. For a state write, the caller already holds
// state.json.lock; for a send or attach, it already owns the project claim.
// Provisioned supervisors have a separate definition path and must not be
// mistaken for missing configured projects.
func (in *Inbox) requireCurrentDefinitions(projects []Project) error {
	if in.configPath == "" {
		return nil
	}
	cfg, err := config.Load(in.configPath)
	if err != nil {
		return fmt.Errorf("cannot verify current project definitions: %w", err)
	}
	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("current configuration is invalid: %w", err)
	}
	definitions := make(map[string]config.Project, len(cfg.Projects))
	for _, p := range cfg.Projects {
		definitions[ident.Name(p.Name)] = p
	}
	for _, p := range projects {
		if in.IsKing(p.Name) {
			continue
		}
		current, ok := definitions[ident.Name(p.Name)]
		if !ok || current.Tool != p.Tool || !ident.SameDir(current.Dir, p.Dir) {
			return fmt.Errorf(
				"project %q changed in config; reload this frontend before writing or sending",
				p.Name,
			)
		}
	}
	return nil
}
