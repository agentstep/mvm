package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// parseEnvFile reads KEY=VALUE lines from path, skipping blank lines and
// full-line comments (a leading '#'), matching Apple container's documented
// --env-file format. Each valid line is returned as a "KEY=VALUE" string in
// the exact shape mvm exec/run's existing -e/--env flag already produces, so
// callers just append the result to their envVars slice.
//
// Unlike Docker's --env-file, a bare KEY (no '=') is a hard error rather
// than silently inheriting the value from mvm's own host environment —
// leaking unrelated host state into the guest is exactly the class of leak
// mvm's sandbox model exists to prevent, so it is never done implicitly.
func parseEnvFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read env file %q: %w", path, err)
	}
	defer f.Close()

	var result []string
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "=") {
			return nil, fmt.Errorf("%s:%d: invalid line %q (want KEY=VALUE)", path, lineNum, line)
		}
		result = append(result, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read env file %q: %w", path, err)
	}
	return result, nil
}

// mergeEnvFile combines --env-file entries with explicit -e/--env values,
// file entries first so explicit flags override same-key file entries —
// matching Docker's --env-file + -e precedence (later `export` wins in the
// shell script buildExecScript assembles). A no-op (returns explicit
// unchanged) when path is "".
func mergeEnvFile(path string, explicit []string) ([]string, error) {
	if path == "" {
		return explicit, nil
	}
	fileVars, err := parseEnvFile(path)
	if err != nil {
		return nil, err
	}
	return append(fileVars, explicit...), nil
}
