package httpapi

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestOpenAPICoversEveryJSONAPIRoute(t *testing.T) {
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatalf("decode embedded OpenAPI document: %v", err)
	}
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read route declarations: %v", err)
	}
	pattern := regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+) (/api/[^" ]+)"`)
	excluded := map[string]bool{
		"GET /api/docs":         true,
		"GET /api/openapi.json": true,
	}
	for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
		method, path := match[1], strings.ReplaceAll(match[2], "{$}", "")
		if excluded[method+" "+path] {
			continue
		}
		operations, ok := document.Paths[path]
		if !ok {
			t.Errorf("OpenAPI document is missing %s %s", method, path)
			continue
		}
		if _, ok := operations[strings.ToLower(method)]; !ok {
			t.Errorf("OpenAPI document is missing %s operation for %s", method, path)
		}
	}
}
