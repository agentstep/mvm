package cli

import "fmt"

// resolveFormat reconciles the container-style --format flag with list's
// pre-existing --json boolean. --format is purely additive: leaving it at
// its zero value "" keeps --json working exactly as it always has (whatever
// jsonFlag says wins). A non-empty --format takes precedence over --json
// when both are given — deliberately simple rather than treating that
// combination as a conflict to reject, since a cosmetic double-flag isn't
// worth the extra cobra flag-changed tracking.
func resolveFormat(format string, jsonFlag bool) (wantJSON bool, err error) {
	switch format {
	case "":
		return jsonFlag, nil
	case "json":
		return true, nil
	case "table":
		return false, nil
	default:
		return false, fmt.Errorf("invalid --format %q (want %q or %q)", format, "json", "table")
	}
}
