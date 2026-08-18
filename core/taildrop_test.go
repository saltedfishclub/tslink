package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestResolveTaildropDisabledWhenMissing(t *testing.T) {
	dir, enabled, err := resolveTaildrop(Feature{})
	if err != nil {
		t.Fatal(err)
	}
	if enabled || dir != "" {
		t.Fatalf("got dir=%q enabled=%v, want disabled", dir, enabled)
	}
}

func TestResolveTaildropRequiresDirectory(t *testing.T) {
	_, _, err := resolveTaildrop(Feature{Taildrop: []Taildrop{{}}})
	if err == nil {
		t.Fatal("expected error for empty directory")
	}
}

func TestResolveTaildropRejectsMultiple(t *testing.T) {
	_, _, err := resolveTaildrop(Feature{Taildrop: []Taildrop{
		{Directory: "/a"},
		{Directory: "/b"},
	}})
	if err == nil {
		t.Fatal("expected error for multiple sections")
	}
}

func TestResolveTaildropAbsAndExpand(t *testing.T) {
	t.Setenv("TSLINK_TAILDROP_TEST", "recv")
	dir, enabled, err := resolveTaildrop(Feature{Taildrop: []Taildrop{
		{Directory: filepath.Join("$TSLINK_TAILDROP_TEST", "inbox")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("expected enabled")
	}
	want, err := filepath.Abs(filepath.Join("recv", "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	if dir != want {
		t.Fatalf("dir=%q want=%q", dir, want)
	}
}

func TestLoadFeatureTaildropTOML(t *testing.T) {
	var cfg Config
	_, err := toml.Decode(`
[[feature.taildrop]]
directory = "/data/Taildrop"
`, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Feature.Taildrop) != 1 || cfg.Feature.Taildrop[0].Directory != "/data/Taildrop" {
		t.Fatalf("unexpected config: %+v", cfg.Feature)
	}
}

func TestResolveTaildropKeepsAbsolute(t *testing.T) {
	want := filepath.Join(os.TempDir(), "taildrop-abs")
	dir, enabled, err := resolveTaildrop(Feature{Taildrop: []Taildrop{
		{Directory: want},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !enabled || dir != want {
		t.Fatalf("dir=%q enabled=%v want %q", dir, enabled, want)
	}
}
