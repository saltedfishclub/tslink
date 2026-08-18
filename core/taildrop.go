package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"tailscale.com/envknob"
	"tailscale.com/feature/taildrop"
	"tailscale.com/ipn/ipnext"
	"tailscale.com/types/logger"
)

func init() {
	if err := wrapTaildropConstructor(); err != nil {
		panic(err)
	}
}

var configuredTaildropDir string

func enableTaildrop(dir string) {
	configuredTaildropDir = dir
}

// wrapTaildropConstructor injects SetDirectFileRoot at extension construction
// time. Official taildrop.Init creates the manager during LocalBackend.Start,
// so the directory must already be set before that Init runs. tsnet does not
// expose LocalBackend, and our own extension would Init after taildrop.
func wrapTaildropConstructor() error {
	for def := range ipnext.Extensions() {
		if def.Name() != "taildrop" {
			continue
		}
		fnField := reflect.ValueOf(def).Elem().FieldByName("newFn")
		if !fnField.IsValid() {
			return errors.New("taildrop extension: newFn field not found")
		}
		writable := reflect.NewAt(fnField.Type(), fnField.Addr().UnsafePointer()).Elem()
		original, ok := writable.Interface().(ipnext.NewExtensionFn)
		if !ok {
			return errors.New("taildrop extension: unexpected newFn type")
		}
		writable.Set(reflect.ValueOf(func(logf logger.Logf, sb ipnext.SafeBackend) (ipnext.Extension, error) {
			ext, err := original(logf, sb)
			if err != nil {
				return ext, err
			}
			if configuredTaildropDir == "" {
				return ext, nil
			}
			td, ok := ext.(*taildrop.Extension)
			if !ok {
				return nil, fmt.Errorf("taildrop extension: unexpected type %T", ext)
			}
			td.SetDirectFileRoot(configuredTaildropDir)
			return ext, nil
		}))
		return nil
	}
	return errors.New("taildrop extension is not registered")
}

func resolveTaildrop(cfg Feature) (dir string, enabled bool, err error) {
	switch len(cfg.Taildrop) {
	case 0:
		return "", false, nil
	case 1:
		d := strings.TrimSpace(os.ExpandEnv(cfg.Taildrop[0].Directory))
		if d == "" {
			return "", false, errors.New("feature.taildrop.directory is required")
		}
		if !filepath.IsAbs(d) {
			d, err = filepath.Abs(d)
			if err != nil {
				return "", false, fmt.Errorf("feature.taildrop.directory: %w", err)
			}
		}
		return d, true, nil
	default:
		return "", false, errors.New("only one [[feature.taildrop]] section is allowed")
	}
}

func applyTaildropConfig(cfg Feature) (dir string, enabled bool, err error) {
	dir, enabled, err = resolveTaildrop(cfg)
	if err != nil {
		return "", false, err
	}
	if !enabled {
		configuredTaildropDir = ""
		envknob.Setenv("TS_DISABLE_TAILDROP", "true")
		return "", false, nil
	}
	envknob.Setenv("TS_DISABLE_TAILDROP", "")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", false, fmt.Errorf("create taildrop directory: %w", err)
	}
	enableTaildrop(dir)
	return dir, true, nil
}
