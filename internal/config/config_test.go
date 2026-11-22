package config

import (
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
	vs := Resolve(settings, fromFile)
	want := map[string][2]string{
		"T_HOST":     {"from-env", "environment"}, // the environment wins over the file
		"T_PASSWORD": {"pw", "env file"},
		"T_OPTIONAL": {"", "none"},
		"T_INTERVAL": {"30s", "env file"},
		"T_TIMEOUT":  {"10s", "default"},
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
	empty := Resolve(settings[:2], nil)
	if err := empty.CheckRequired(); err == nil || !strings.Contains(err.Error(), "T_HOST, T_PASSWORD") {
		t.Errorf("CheckRequired on empty = %v", err)
	}
	if _, err := (Values{{Setting: Setting{Name: "X"}, Value: "soon"}}).Duration("X"); err == nil {
		t.Error("bad duration accepted")
	}
}
