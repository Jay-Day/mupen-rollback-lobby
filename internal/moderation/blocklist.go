package moderation

import (
	"bufio"
	_ "embed"
	"os"
	"strings"
)

//go:embed blocklist.txt
var defaultBlocklist string

// DefaultTerms returns the blocklist compiled into the binary.
func DefaultTerms() []string { return parseTerms(defaultBlocklist) }

// LoadBlocklistFile reads an override blocklist (one term per line; '#'
// comments and blank lines ignored).
func LoadBlocklistFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseTerms(string(data)), nil
}

func parseTerms(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}
