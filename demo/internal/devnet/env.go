package devnet

import (
	"bufio"
	"errors"
	"os"
	"strings"
)

// Env looks a variable up in the environment first, then in the dotenv files
// given, in order. The demo's programs are run with `go run`, usually without
// the repository's .env exported, and the faucet needs its CDP key.
//
// A file that does not exist is skipped. Each line is KEY=VALUE, optionally
// after `export `, with the value optionally in matching quotes; blank lines
// and lines starting with # are ignored. That is the subset .env.example uses,
// not every shell's syntax.
func Env(files ...string) (func(name string) string, error) {
	values := map[string]string{}
	for i := len(files) - 1; i >= 0; i-- {
		parsed, err := readDotenv(files[i])
		if err != nil {
			return nil, err
		}
		for k, v := range parsed {
			values[k] = v
		}
	}
	return func(name string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return values[name]
	}, nil
}

func readDotenv(path string) (map[string]string, error) {
	f, err := os.Open(path) // #nosec G304 -- a dotenv file the person running the demo names
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	return values, scanner.Err()
}

// unquote strips one pair of matching surrounding quotes.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}
