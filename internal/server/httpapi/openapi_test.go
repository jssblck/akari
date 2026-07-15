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

func TestOpenAPICoreBoundarySchemasAreClosed(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]struct {
				AdditionalProperties *bool          `json:"additionalProperties"`
				Properties           map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatalf("decode embedded OpenAPI document: %v", err)
	}
	for _, name := range []string{
		"Error", "Publication", "Login", "Register", "CreateToken",
		"CreateInvite", "AnnounceSession", "Viewer", "ProjectionRebuild",
	} {
		schema := document.Components.Schemas[name]
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("schema %s must reject undocumented fields", name)
		}
		if len(schema.Properties) == 0 {
			t.Errorf("schema %s must declare its fields", name)
		}
	}
}

func TestOpenAPIDocumentsProjectionRebuildResponses(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Ref string `json:"$ref"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatalf("decode embedded OpenAPI document: %v", err)
	}
	for _, path := range []string{
		"/api/v1/app/overview",
		"/api/v1/app/insights",
		"/api/v1/app/projects",
		"/api/v1/app/projects/{id}",
		"/api/v1/app/sessions",
		"/api/v1/app/sessions/{id}",
		"/api/v1/app/sessions/{id}/append",
		"/api/v1/app/sessions/{id}/transcript",
		"/api/v1/app/public/users/{username}",
		"/api/v1/app/public/projects/{id}",
		"/api/v1/app/public/sessions/{public_id}",
		"/api/v1/app/public/sessions/{public_id}/transcript",
	} {
		response, ok := document.Paths[path]["get"].Responses["503"]
		if !ok || response.Ref != "#/components/responses/ProjectionRebuilding" {
			t.Errorf("GET %s does not document ProjectionRebuilding", path)
		}
	}
}

func TestOpenAPIDocumentsEverySessionFilterParameter(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name   string `json:"name"`
				In     string `json:"in"`
				Schema struct {
					Enum []string `json:"enum"`
				} `json:"schema"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatalf("decode embedded OpenAPI document: %v", err)
	}
	documented := make(map[string]bool)
	enums := make(map[string][]string)
	for _, parameter := range document.Paths["/api/v1/app/sessions"]["get"].Parameters {
		if parameter.In == "query" {
			documented[parameter.Name] = true
			enums[parameter.Name] = parameter.Schema.Enum
		}
	}
	for key := range sessionFilterQueryKeys {
		if !documented[key] {
			t.Errorf("session filter %q is missing from OpenAPI", key)
		}
	}
	for key := range documented {
		if _, ok := sessionFilterQueryKeys[key]; !ok {
			t.Errorf("OpenAPI documents unsupported session filter %q", key)
		}
	}
	for name, accepted := range map[string]map[string]struct{}{
		"sort": apiSessionSortKeys, "grade": apiGradeKeys,
		"outcome": apiOutcomeKeys, "range": apiRangeKeys,
	} {
		documentedValues := make(map[string]bool, len(enums[name]))
		for _, value := range enums[name] {
			documentedValues[value] = true
		}
		for value := range accepted {
			if !documentedValues[value] {
				t.Errorf("%s value %q is accepted but missing from OpenAPI", name, value)
			}
		}
		for value := range documentedValues {
			if _, ok := accepted[value]; !ok {
				t.Errorf("OpenAPI documents unsupported %s value %q", name, value)
			}
		}
	}
}
