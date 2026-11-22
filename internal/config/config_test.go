package config

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisplayMasksSecrets(t *testing.T) {
	for _, tc := range []struct {
		v    Value
		want string
	}{
		{Value{Setting: Setting{Name: "KTC_HOST"}, Value: "192.168.1.100"}, "192.168.1.100"},
		{Value{Setting: Setting{Name: "KTC_PASSWORD", Secret: true}, Value: "pw"}, "*****"},
		{Value{Setting: Setting{Name: "ELVA_DB_PASSWORD"}, Value: "pw"}, "*****"}, // secret by name
		{Value{Setting: Setting{Name: "ELVA_DATABASE_URL"}, Value: "postgres://elva:pw@db:5432/elva"}, "postgres://elva:xxxxx@db:5432/elva"},
		{Value{Setting: Setting{Name: "KTC_METERS"}}, "(not set)"},
	} {
		if got := tc.v.Display(); got != tc.want {
			t.Errorf("%s: Display() = %q, want %q", tc.v.Name, got, tc.want)
		}
	}
}

func TestResolve(t *testing.T) {
	settings := []Setting{
		{Name: "T_HOST", Required: true},
		{Name: "T_PASSWORD", Required: true},
		{Name: "T_OPTIONAL"},
		{Name: "T_INTERVAL", Default: "1m"},
		{Name: "T_TIMEOUT", Default: "10s"},
	}
	for _, s := range settings {
		t.Setenv(s.Name, "") // restore after the test
		os.Unsetenv(s.Name)
	}
	t.Setenv("T_HOST", "from-env")
	env := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(env, []byte("# comment\nT_HOST=from-file\nexport T_PASSWORD='pw'\nT_INTERVAL=30s\n"), 0o600)

	fromFile, err := LoadEnvFile(env)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("T_TIMEOUT", "20s")
	flags := map[string]string{"T_TIMEOUT": "5s", "T_INTERVAL": "", "T_OPTIONAL": "x"}
	vs := Resolve(settings, flags, fromFile)
	want := map[string][2]string{
		"T_HOST":     {"from-env", "environment"}, // the environment wins over the file
		"T_PASSWORD": {"pw", "env file"},
		"T_OPTIONAL": {"x", "flag"},
		"T_INTERVAL": {"30s", "env file"}, // an empty flag counts as not given
		"T_TIMEOUT":  {"5s", "flag"},      // a flag wins over the environment
	}
	for _, v := range vs {
		if w := want[v.Name]; v.Value != w[0] || v.Source != w[1] {
			t.Errorf("%s = %q from %q, want %q from %q", v.Name, v.Value, v.Source, w[0], w[1])
		}
	}
	if err := vs.CheckRequired(); err != nil {
		t.Errorf("CheckRequired: %v", err)
	}
	if d, err := vs.Duration("T_INTERVAL"); err != nil || d.Seconds() != 30 {
		t.Errorf("Duration = %v, %v", d, err)
	}

	os.Unsetenv("T_HOST") // T_PASSWORD was set by the env file
	os.Unsetenv("T_PASSWORD")
	empty := Resolve(settings[:2], nil, nil)
	if err := empty.CheckRequired(); err == nil || !strings.Contains(err.Error(), "T_HOST, T_PASSWORD") {
		t.Errorf("CheckRequired on empty = %v", err)
	}
	if _, err := (Values{{Setting: Setting{Name: "X"}, Value: "soon"}}).Duration("X"); err == nil {
		t.Error("bad duration accepted")
	}
}

func TestFlags(t *testing.T) {
	settings := []Setting{
		{Name: "ELVA_QUERY_MAX_ROWS", Default: "1000", Help: "row limit"},
		{Name: "KTC_HOST", Required: true, Help: "RCU host"},
		{Name: "KTC_PASSWORD"},
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	check := fs.Bool("check", false, "run checks")
	given := Flags(fs, settings)
	if err := fs.Parse([]string{"-check", "-ktc-host", "rcu", "-query-max-rows=5"}); err != nil {
		t.Fatal(err)
	}
	got := given()
	if !*check || len(got) != 2 || got["KTC_HOST"] != "rcu" || got["ELVA_QUERY_MAX_ROWS"] != "5" {
		t.Errorf("given = %v, check = %v", got, *check)
	}

	var help bytes.Buffer
	PrintHelp(&help, fs, settings)
	for _, want := range []string{"  -check\n        run checks", "  -query-max-rows, ELVA_QUERY_MAX_ROWS (default 1000)\n        row limit",
		"  -ktc-host, KTC_HOST (required)"} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, help.String())
		}
	}
	if strings.Contains(help.String(), "  -ktc-host string") {
		t.Errorf("setting flags listed twice:\n%s", help.String())
	}
}
