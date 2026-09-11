package config

import (
	"fmt"
	"path/filepath"
)

// validateAbsoluteDirs refuses identities whose meaning changes with the
// working directory the CLI happens to be launched from. A relative dir in
// a hand-written config resolves against today's cwd, so the same persisted
// session can be restored into a different repository tomorrow; resolution
// at add-time does not cover legacy or hand-edited configuration, so
// validation is the boundary that does.
func validateAbsoluteDirs(s *Settings) error {
	for i, p := range s.Projects {
		if !filepath.IsAbs(p.Dir) {
			return fmt.Errorf("project %d (%s): dir must be absolute, got %q", i+1, p.Name, p.Dir)
		}
	}
	if s.King.Dir != "" && !filepath.IsAbs(s.King.Dir) {
		return fmt.Errorf("king.dir must be absolute, got %q", s.King.Dir)
	}
	for _, g := range s.Groups {
		if g.King.Dir != "" && !filepath.IsAbs(g.King.Dir) {
			return fmt.Errorf("group %q: king.dir must be absolute, got %q", g.Name, g.King.Dir)
		}
	}
	return nil
}
