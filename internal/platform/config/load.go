package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	envPrefix      = "MOUNTENANT_"
	envConfigPath  = "MOUNTENANT_CONFIG"
	envUsersPath   = "MOUNTENANT_USERS"
	fileSuffix     = "_FILE"
	defaultCfgPath = "/etc/mountenant/config.json"
)

// Loaded is the result of Load.
type Loaded struct {
	Config     Config
	Users      []User
	ConfigPath string // empty when no config.json was read
	UsersPath  string // empty when no users.json was read
	Warnings   []string
}

// Load reads configuration in the precedence order of spec §8.1 and validates
// it. environ is os.Environ() in production. Every problem found is returned
// in one joined error.
func Load(environ []string) (*Loaded, error) {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, envPrefix) {
			env[k] = v
		}
	}
	l := &Loaded{Config: Default()}

	// 2. config.json. A missing file at the default path is fine (env-only
	// deployments); a missing file that was asked for is not.
	cfgPath, explicit := env[envConfigPath], true
	if cfgPath == "" {
		cfgPath, explicit = defaultCfgPath, false
	}
	switch b, err := os.ReadFile(cfgPath); {
	case err == nil:
		if err := decodeStrict(b, &l.Config); err != nil {
			return nil, fmt.Errorf("%s: %w", cfgPath, err)
		}
		l.ConfigPath = cfgPath
	case errors.Is(err, fs.ErrNotExist) && !explicit:
	default:
		return nil, fmt.Errorf("config file: %w", err)
	}

	// 3. users.json, next to config.json unless set.
	usersPath, explicit := env[envUsersPath], true
	if usersPath == "" {
		usersPath, explicit = filepath.Join(filepath.Dir(cfgPath), "users.json"), false
	}
	switch b, err := os.ReadFile(usersPath); {
	case err == nil:
		if err := decodeStrict(b, &l.Users); err != nil {
			return nil, fmt.Errorf("%s: %w", usersPath, err)
		}
		l.UsersPath = usersPath
		if fi, err := os.Stat(usersPath); err == nil && fi.Mode().Perm()&0o004 != 0 {
			l.Warnings = append(l.Warnings, usersPath+" is world-readable; use mode 0600")
		}
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		l.Warnings = append(l.Warnings, "no users file at "+usersPath+"; nobody can log in")
	default:
		return nil, fmt.Errorf("users file: %w", err)
	}

	// 4. and 5. environment variables and *_FILE secrets.
	envErr := applyEnv(&l.Config, env)

	w, err := l.Config.Validate()
	l.Warnings = append(l.Warnings, w...)
	err = errors.Join(envErr, err, validateUsers(l.Users))
	if err != nil {
		return nil, err
	}
	return l, nil
}

// decodeStrict decodes one JSON value and rejects unknown keys and trailing
// data.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the top-level value")
	}
	return nil
}

// envField is a config value reachable through an environment variable.
type envField struct {
	jsonPath string
	v        reflect.Value
}

// envFields maps MOUNTENANT_<SECTION>_<KEY> names to config values. Nested
// objects add a segment (logging.securityLog.path is
// MOUNTENANT_LOGGING_SECURITY_LOG_PATH); arrays are leaves and take JSON (or
// a comma-separated list for string arrays).
func envFields(c *Config) map[string]envField {
	out := map[string]envField{}
	var walk func(v reflect.Value, env, path string)
	walk = func(v reflect.Value, env, path string) {
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			tag := sf.Tag.Get("env")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
			seg := tag
			if seg == "" {
				seg = screamingSnake(name)
			}
			f := v.Field(i)
			if f.Kind() == reflect.Struct && f.Type() != reflect.TypeFor[Duration]() {
				walk(f, env+seg+"_", path+name+".")
				continue
			}
			out[env+seg] = envField{jsonPath: path + name, v: f}
		}
	}
	walk(reflect.ValueOf(c).Elem(), envPrefix, "")
	return out
}

// screamingSnake turns "publicUrl" into "PUBLIC_URL".
func screamingSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

func applyEnv(c *Config, env map[string]string) error {
	fields := envFields(c)
	var errs errList
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, k := range names {
		if k == envConfigPath || k == envUsersPath {
			continue
		}
		val := env[k]
		name := k
		if base, ok := strings.CutSuffix(k, fileSuffix); ok {
			if _, known := fields[base]; known {
				if _, both := env[base]; both {
					errs.addf("%s and %s are both set; use one", base, k)
					continue
				}
				b, err := os.ReadFile(val)
				if err != nil {
					errs.addf("%s: %v", k, err)
					continue
				}
				val, name = strings.TrimRight(string(b), "\r\n"), base
			}
		}
		f, ok := fields[name]
		if !ok {
			errs.addf("%s: unknown setting", k)
			continue
		}
		if err := setFromString(f.v, val); err != nil {
			// The value may be a secret, so it is never echoed.
			errs.addf("%s (%s): %v", k, f.jsonPath, err)
		}
	}
	return errs.err()
}

func setFromString(v reflect.Value, s string) error {
	switch p := v.Addr().Interface().(type) {
	case *Duration:
		d, err := time.ParseDuration(s)
		if err != nil {
			return errors.New("not a duration like \"15m\"")
		}
		p.Duration = d
		return nil
	case *[]string:
		if strings.HasPrefix(strings.TrimSpace(s), "[") {
			return decodeStrict([]byte(s), p)
		}
		var out []string
		for _, part := range strings.Split(s, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		*p = out
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return errors.New("not a boolean")
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v.OverflowInt(n) {
			return errors.New("not an integer")
		}
		v.SetInt(n)
	case reflect.Uint8, reflect.Uint32:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil || v.OverflowUint(n) {
			return errors.New("not an unsigned integer in range")
		}
		v.SetUint(n)
	case reflect.Slice:
		fresh := reflect.New(v.Type())
		if err := decodeStrict([]byte(s), fresh.Interface()); err != nil {
			return errors.New("not valid JSON for this array")
		}
		v.Set(fresh.Elem())
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
	return nil
}
