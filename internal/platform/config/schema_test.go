package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const configsDir = "../../../configs"

type schemaDoc map[string]any

func readSchema(t *testing.T, name string) schemaDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(configsDir, name))
	if err != nil {
		t.Fatal(err)
	}
	var s schemaDoc
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s
}

// resolve follows a local or cross-file $ref.
func resolve(t *testing.T, docs map[string]schemaDoc, file string, node map[string]any) (string, map[string]any) {
	ref, ok := node["$ref"].(string)
	if !ok {
		return file, node
	}
	f, ptr, _ := strings.Cut(ref, "#")
	if f != "" {
		file = f
	}
	cur := any(map[string]any(docs[file]))
	for _, seg := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		cur = cur.(map[string]any)[seg]
	}
	return resolve(t, docs, file, cur.(map[string]any))
}

// TestSchemaMatchesStructs keeps the published JSON Schemas and the Go
// structs in step: every JSON key of a struct must be a schema property and
// vice versa, and every object must forbid additional properties, like the
// loader does.
func TestSchemaMatchesStructs(t *testing.T) {
	docs := map[string]schemaDoc{
		"config.schema.json": readSchema(t, "config.schema.json"),
		"users.schema.json":  readSchema(t, "users.schema.json"),
	}
	var check func(path, file string, node map[string]any, typ reflect.Type)
	check = func(path, file string, node map[string]any, typ reflect.Type) {
		file, node = resolve(t, docs, file, node)
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			if typ.Kind() == reflect.Slice {
				items, ok := node["items"].(map[string]any)
				if !ok {
					t.Errorf("%s: schema has no items for %s", path, typ)
					return
				}
				file, node = resolve(t, docs, file, items)
				path += "[]"
			}
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || typ == reflect.TypeFor[Duration]() {
			return
		}
		if node["additionalProperties"] != false {
			t.Errorf("%s: additionalProperties must be false", path)
		}
		props, _ := node["properties"].(map[string]any)
		want := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			want[name] = typ.Field(i).Type
		}
		var missing, extra []string
		for name, ft := range want {
			p, ok := props[name].(map[string]any)
			if !ok {
				missing = append(missing, name)
				continue
			}
			check(path+"."+name, file, p, ft)
		}
		for name := range props {
			if _, ok := want[name]; !ok {
				extra = append(extra, name)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 || len(extra) > 0 {
			t.Errorf("%s: missing in schema %v, unknown to Go %v", path, missing, extra)
		}
	}
	check("config", "config.schema.json", docs["config.schema.json"], reflect.TypeFor[Config]())
	check("users", "users.schema.json", docs["users.schema.json"], reflect.TypeFor[[]User]())
}

// TestSchemaDefaultsMatchCode makes sure the defaults documented in the
// schema are the ones the code applies.
func TestSchemaDefaultsMatchCode(t *testing.T) {
	docs := map[string]schemaDoc{
		"config.schema.json": readSchema(t, "config.schema.json"),
		"users.schema.json":  readSchema(t, "users.schema.json"),
	}
	b, _ := json.Marshal(Default())
	var defaults map[string]any
	json.Unmarshal(b, &defaults)

	var walk func(path, file string, node map[string]any, val any)
	walk = func(path, file string, node map[string]any, val any) {
		// A default may sit next to a $ref, so look before resolving.
		if d, ok := node["default"]; ok && !reflect.DeepEqual(normDuration(d), normDuration(val)) {
			t.Errorf("%s: schema default %v, code default %v", path, d, val)
		}
		file, node = resolve(t, docs, file, node)
		props, _ := node["properties"].(map[string]any)
		obj, _ := val.(map[string]any)
		for name, p := range props {
			walk(path+"."+name, file, p.(map[string]any), obj[name])
		}
	}
	walk("config", "config.schema.json", docs["config.schema.json"], defaults)
}

// normDuration makes "12h" and "12h0m0s" compare equal.
func normDuration(v any) any {
	if s, ok := v.(string); ok {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return v
}

func TestExamplesAreValid(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys.json")
	os.WriteFile(keys, []byte(`[{"id":"k1","secret":"`+secret32+`"}]`), 0o600)
	l, err := Load([]string{
		"MOUNTENANT_CONFIG=" + filepath.Join(configsDir, "config.example.json"),
		"MOUNTENANT_USERS=" + filepath.Join(configsDir, "users.example.json"),
		"MOUNTENANT_SIGNING_KEYS_FILE=" + keys,
		"MOUNTENANT_BACKEND_API_KEY=key",
		"MOUNTENANT_BACKEND_WEBDAV_PASSWORD=pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Users) != 2 {
		t.Fatalf("users %d", len(l.Users))
	}
}
