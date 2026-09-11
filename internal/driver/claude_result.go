package driver

import "fmt"

// validateClaudeResult fails closed when a terminal frame does not
// explicitly identify success. is_error=true remains an error even if the
// subtype claims success; an omitted subtype is not a success — a truncated
// or protocol-drifted result frame must not promote intermediate text into
// a completed answer.
func validateClaudeResult(r claudeResult) error {
	if r.IsError {
		return fmt.Errorf("claude: %s", r.Subtype)
	}
	if r.Subtype != "success" {
		return fmt.Errorf("claude: invalid or unsuccessful result subtype %q", r.Subtype)
	}
	return nil
}
