package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type repairFixture struct {
	Name        string          `json:"name"`
	Tool        string          `json:"tool"`
	Input       json.RawMessage `json:"input"`
	InputString string          `json:"input_string"`
	WantClasses []string        `json:"want_classes"`
	WantPath    string          `json:"want_path"`
	WantError   string          `json:"want_error"`
}

func TestRepairFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/repairs/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var f repairFixture
			if err := json.Unmarshal(b, &f); err != nil {
				t.Fatalf("bad fixture: %v", err)
			}
			raw := f.InputString
			if raw == "" && f.Input != nil {
				raw = string(f.Input)
			}

			got, err := Repair(f.Tool, raw)
			if f.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), f.WantError) {
					t.Fatalf("%s: want error containing %q, got %v", f.Name, f.WantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", f.Name, err)
			}
			for _, want := range f.WantClasses {
				if !contains(got.Classes, want) {
					t.Fatalf("%s: classes %v missing %q", f.Name, got.Classes, want)
				}
			}
			if f.WantPath != "" {
				var obj map[string]any
				if err := json.Unmarshal([]byte(got.Args), &obj); err != nil {
					t.Fatalf("%s: repaired args not JSON: %v", f.Name, err)
				}
				if obj["path"] != f.WantPath {
					t.Fatalf("%s: path = %v, want %q", f.Name, obj["path"], f.WantPath)
				}
			}
		})
	}
}

func TestRepairArrayShapes(t *testing.T) {
	spec := Spec{Required: []string{"items"}, Types: map[string]string{"items": "array"}}

	got, err := repairWith("demo", spec, `{"items":"[\"a\",\"b\"]"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got.Classes, "stringified-array") {
		t.Fatalf("stringified array not repaired: %v", got.Classes)
	}

	got, err = repairWith("demo", spec, `{"items":"single"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got.Classes, "scalar-array") {
		t.Fatalf("scalar array not repaired: %v", got.Classes)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(got.Args), &obj); err != nil {
		t.Fatal(err)
	}
	if arr, ok := obj["items"].([]any); !ok || len(arr) != 1 || arr[0] != "single" {
		t.Fatalf("scalar not wrapped: %#v", obj["items"])
	}
}

func TestRepairRejectsNonObject(t *testing.T) {
	if _, err := Repair("read", `[1,2]`); err == nil {
		t.Fatal("multi-element array accepted")
	}
	if _, err := Repair("read", `42`); err == nil {
		t.Fatal("numeric root accepted")
	}
}
