// Package config loads settings from the environment and an optional env file,
// logs them with secrets masked, and prints -help text. Each binary declares its
// own []Setting table; that table is the single place a setting is defined.
package config

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"
)

// Setting describes one configuration variable.
type Setting struct {
	Name     string
	Default  string // empty means none
	Required bool
	Secret   bool // never logged; names containing PASSWORD/SECRET/TOKEN are treated as secret too
	Help     string
}

// Value is a setting's effective value and where it came from.
type Value struct {
	Setting
	Value  string
	Source string // "environment", "env file", "default" or "none"
}

// Values are the resolved settings of one binary.
type Values []Value

// Load reads envFile (if not empty) into the environment, resolves every setting
// and logs them with secrets masked.
func Load(log *slog.Logger, settings []Setting, envFile string) (Values, error) {
	var fromFile map[string]bool
	if envFile != "" {
		var err error
		if fromFile, err = LoadEnvFile(envFile); err != nil {
			return nil, err
		}
		log.Info("loaded env file", "path", envFile)
	}
	vs := Resolve(settings, fromFile)
	for _, v := range vs {
		log.Info("setting", "name", v.Name, "value", v.Display(), "source", v.Source)
	}
	return vs, nil
}

// Resolve looks up every setting. fromFile holds the names set by the env file.
func Resolve(settings []Setting, fromFile map[string]bool) Values {
	out := make(Values, len(settings))
	for i, s := range settings {
		v := Value{Setting: s, Source: "none"}
		if x, ok := os.LookupEnv(s.Name); ok && x != "" {
			v.Value, v.Source = x, "environment"
			if fromFile[s.Name] {
				v.Source = "env file"
			}
		} else if s.Default != "" {
			v.Value, v.Source = s.Default, "default"
		}
		out[i] = v
	}
	return out
}

// Display returns the value as it may be logged.
func (v Value) Display() string {
	switch {
	case v.Value == "":
		return "(not set)"
	case v.Secret || looksSecret(v.Name):
		return "*****"
	}
	// Connection strings like postgres://user:pw@host/db carry a password.
	if u, err := url.Parse(v.Value); err == nil && u.User != nil {
		if _, ok := u.User.Password(); ok {
			return u.Redacted()
		}
	}
	return v.Value
}

func looksSecret(name string) bool {
	for _, w := range []string{"PASSWORD", "SECRET", "TOKEN"} {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

// CheckRequired returns an error naming every required setting that is empty.
func (vs Values) CheckRequired() error {
	var missing []string
	for _, v := range vs {
		if v.Required && v.Value == "" {
			missing = append(missing, v.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required setting(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// Get returns the value of the named setting ("" if unknown).
func (vs Values) Get(name string) string {
	for _, v := range vs {
		if v.Name == name {
			return v.Value
		}
	}
	return ""
}

// Duration parses the named setting as a positive duration.
func (vs Values) Duration(name string) (time.Duration, error) {
	d, err := time.ParseDuration(vs.Get(name))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s=%q: must be a positive duration like 1m or 30s", name, vs.Get(name))
	}
	return d, nil
}

// Location parses the named setting as an IANA time zone, e.g. Europe/Stockholm.
func (vs Values) Location(name string) (*time.Location, error) {
	loc, err := time.LoadLocation(vs.Get(name))
	if err != nil {
		return nil, fmt.Errorf("%s=%q: %v", name, vs.Get(name), err)
	}
	return loc, nil
}

// LoadEnvFile sets KEY=VALUE lines from path, skipping blanks and # comments, and
// returns the names it set. Variables already present in the environment win.
func LoadEnvFile(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	set := map[string]bool{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
			set[k] = true
		}
	}
	return set, sc.Err()
}

// PrintHelp writes the settings table for -help.
func PrintHelp(w io.Writer, settings []Setting) {
	fmt.Fprintln(w, "\nSettings (environment variables; also accepted in the -env-file file):")
	for _, s := range settings {
		extra := ""
		switch {
		case s.Required:
			extra = " (required)"
		case s.Default != "":
			extra = " (default " + s.Default + ")"
		}
		fmt.Fprintf(w, "  %-21s %s%s\n", s.Name, s.Help, extra)
	}
}
