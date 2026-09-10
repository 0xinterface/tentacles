// Package env parses the runner environment file — a systemd
// EnvironmentFile subset — and layers it over a process environment.
package env

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ParseFile reads an EnvironmentFile-format file: KEY=VALUE lines with
// optional surrounding single or double quotes on the value, "#" comment
// lines and blank lines skipped. A line without '=' is an error that names
// the offending line number.
func ParseFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vars := make(map[string]string)
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("env: %s:%d: malformed line %q: expected KEY=VALUE", path, lineNo, line)
		}
		key := strings.TrimSpace(line[:eq])
		val := unquote(strings.TrimSpace(line[eq+1:]))
		vars[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return vars, nil
}

// unquote strips one pair of surrounding single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// Validate reports an error naming every required variable that is
// missing or empty. PATH and HOME are mandatory for the runner agent
// to locate the host toolchain and its caches.
func Validate(vars map[string]string) error {
	var missing []string
	for _, k := range []string{"PATH", "HOME"} {
		if strings.TrimSpace(vars[k]) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("env: %s must be set and non-empty", strings.Join(missing, ", "))
	}
	return nil
}

// Apply returns environ (a list of KEY=VALUE strings) with vars layered on
// top: keys already present are replaced in place, keys that are new are
// appended in sorted order for determinism. The input slice is not mutated.
func Apply(environ []string, vars map[string]string) []string {
	out := make([]string, 0, len(environ)+len(vars))
	seen := make(map[string]bool, len(vars))
	for _, kv := range environ {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if v, ok := vars[key]; ok {
			out = append(out, key+"="+v)
			seen[key] = true
		} else {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+vars[k])
	}
	return out
}
