package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDisplayMasksSecrets(t *testing.T) {
	for _, tc := range []struct {
		r    resolved
		want string
	}{
		{resolved{setting: setting{name: "KTC_HOST"}, value: "192.168.1.100"}, "192.168.1.100"},
		{resolved{setting: setting{name: "KTC_PASSWORD", secret: true}, value: "pw"}, "*****"},
		{resolved{setting: setting{name: "ELVA_DB_PASSWORD"}, value: "pw"}, "*****"}, // secret by name
		{resolved{setting: setting{name: "ELVA_DB_URL"}, value: "postgres://elva:pw@db:5432/elva"}, "postgres://elva:xxxxx@db:5432/elva"},
		{resolved{setting: setting{name: "KTC_METERS"}}, "(not set)"},
	} {
		if got := tc.r.display(); got != tc.want {
			t.Errorf("%s: display() = %q, want %q", tc.r.name, got, tc.want)
		}
	}
}

func TestResolveAndParse(t *testing.T) {
	for _, s := range settings {
		t.Setenv(s.name, "") // isolate from the developer's environment
		os.Unsetenv(s.name)
	}
	t.Setenv("KTC_HOST", "from-env")
	env := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(env, []byte("# comment\nKTC_HOST=from-file\nexport KTC_PASSWORD='pw'\nELVA_INTERVAL=30s\n"), 0o600)

	fromFile, err := loadEnvFile(env)
	if err != nil {
		t.Fatal(err)
	}
	rs := resolveSettings(fromFile)
	src := map[string]string{}
	for _, r := range rs {
		src[r.name] = r.source
	}
	want := map[string]string{"KTC_HOST": "environment", "KTC_PASSWORD": "env file", "KTC_METERS": "none", "ELVA_INTERVAL": "env file", "ELVA_TIMEOUT": "default"}
	for k, v := range want {
		if src[k] != v {
			t.Errorf("%s source = %q, want %q", k, src[k], v)
		}
	}
	cfg, err := parseConfig(rs)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.host != "from-env" || cfg.password != "pw" || cfg.interval != 30*time.Second || cfg.timeout != 10*time.Second || cfg.meters != nil {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestParseConfigErrors(t *testing.T) {
	r := func(name, v string) resolved {
		for _, s := range settings {
			if s.name == name {
				return resolved{setting: s, value: v}
			}
		}
		t.Fatalf("no setting %s", name)
		return resolved{}
	}
	if _, err := parseConfig([]resolved{r("KTC_HOST", ""), r("KTC_PASSWORD", "")}); err == nil || !strings.Contains(err.Error(), "KTC_HOST, KTC_PASSWORD") {
		t.Errorf("missing: err = %v", err)
	}
	if _, err := parseConfig([]resolved{r("KTC_HOST", "h"), r("KTC_PASSWORD", "p"), r("ELVA_INTERVAL", "soon"), r("ELVA_TIMEOUT", "1s")}); err == nil {
		t.Error("bad interval accepted")
	}
}
