package httpapi

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

type browserContract struct {
	path       string
	method     string
	status     string
	schemaName string
	response   any
}

var browserContracts = []browserContract{
	{"/api/v1/auth/login", "post", "200", "LoginResponse", loginResponse{}},
	{"/api/v1/auth/register", "post", "201", "RegisteredUserResponse", registeredUserResponse{}},
	{"/api/v1/auth/logout", "post", "200", "StatusResponse", statusResponse{}},
	{"/api/v1/app/bootstrap", "get", "200", "Viewer", appViewer{}},
	{"/api/v1/app/oauth/authorize", "get", "200", "OAuthConsentResponse", oauthConsentResponse{}},
	{"/api/v1/app/overview", "get", "200", "OverviewResponse", overviewResponse{}},
	{"/api/v1/app/insights", "get", "200", "InsightsResponse", insightsResponse{}},
	{"/api/v1/app/projects", "get", "200", "ProjectsResponse", projectsResponse{}},
	{"/api/v1/app/projects/{id}", "get", "200", "ProjectResponse", projectResponse{}},
	{"/api/v1/app/projects/{id}/publication", "put", "200", "PublicationResponse", publicationResponse{}},
	{"/api/v1/app/sessions", "get", "200", "SessionsResponse", sessionsResponse{}},
	{"/api/v1/app/sessions/{id}", "get", "200", "SessionResponse", sessionResponse{}},
	{"/api/v1/app/sessions/{id}/append", "get", "200", "SessionResponse", sessionResponse{}},
	{"/api/v1/app/sessions/{id}", "delete", "200", "DeletedSessionResponse", deletedSessionResponse{}},
	{"/api/v1/app/sessions/{id}/transcript", "get", "200", "TranscriptResponse", transcriptResponse{}},
	{"/api/v1/app/sessions/{id}/publication", "put", "200", "SessionPublicationResponse", sessionPublicationResponse{}},
	{"/api/v1/app/account", "get", "200", "AccountResponse", accountResponse{}},
	{"/api/v1/app/account/connections/{client_id}", "delete", "200", "RevokedResponse", revokedResponse{}},
	{"/api/v1/app/account/invites/{id}", "delete", "200", "RevokedResponse", revokedResponse{}},
	{"/api/v1/tokens", "get", "200", "TokensResponse", tokensResponse{}},
	{"/api/v1/tokens", "post", "201", "CreatedTokenResponse", createdTokenResponse{}},
	{"/api/v1/tokens/{id}/revoke", "post", "200", "StatusResponse", statusResponse{}},
	{"/api/v1/invites", "post", "201", "CreatedInviteResponse", createdInviteResponse{}},
	{"/api/v1/app/account/overview-publication", "put", "200", "PublicationResponse", publicationResponse{}},
	{"/api/v1/app/reparse", "post", "202", "ReparseStatusResponse", reparseStatusResponse{}},
	{"/api/v1/reparse/status", "get", "200", "ReparseStatusResponse", reparseStatusResponse{}},
	{"/api/v1/app/public/users/{username}", "get", "200", "PublicOverviewResponse", publicOverviewResponse{}},
	{"/api/v1/app/public/projects/{id}", "get", "200", "PublicProjectResponse", publicProjectResponse{}},
	{"/api/v1/app/public/sessions/{public_id}", "get", "200", "PublicSessionResponse", publicSessionResponse{}},
	{"/api/v1/app/public/sessions/{public_id}/transcript", "get", "200", "PublicSessionResponse", publicSessionResponse{}},
	{"/api/v1/app/guide/", "get", "200", "GuideResponse", guideResponse{}},
	{"/api/v1/app/guide/{slug}", "get", "200", "GuideResponse", guideResponse{}},
}

type contractDocument struct {
	Paths      map[string]map[string]contractOperation `json:"paths"`
	Components struct {
		Schemas map[string]any `json:"schemas"`
	} `json:"components"`
}

type contractOperation struct {
	Responses map[string]struct {
		Content map[string]struct {
			Schema map[string]any `json:"schema"`
		} `json:"content"`
	} `json:"responses"`
}

func TestBrowserContractsMatchOpenAPI(t *testing.T) {
	document := readContractDocument(t)
	expected := browserContractSchemas(t)
	for _, contract := range browserContracts {
		operation, ok := document.Paths[contract.path][contract.method]
		if !ok {
			t.Errorf("OpenAPI is missing %s %s", strings.ToUpper(contract.method), contract.path)
			continue
		}
		response, ok := operation.Responses[contract.status]
		if !ok {
			t.Errorf("OpenAPI is missing %s response for %s %s", contract.status, strings.ToUpper(contract.method), contract.path)
			continue
		}
		schema := response.Content["application/json"].Schema
		wantRef := "#/components/schemas/" + contract.schemaName
		if got, _ := schema["$ref"].(string); got != wantRef {
			t.Errorf("%s %s response %s schema = %q, want %q", strings.ToUpper(contract.method), contract.path, contract.status, got, wantRef)
		}
	}

	for name, want := range expected {
		got, ok := document.Components.Schemas[name]
		if !ok {
			t.Errorf("OpenAPI components are missing %s", name)
			continue
		}
		want = normalizeContractSchema(t, want)
		if !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			t.Errorf("OpenAPI schema %s drifted from its Go DTO\n got: %s\nwant: %s", name, gotJSON, wantJSON)
		}
	}
}

func TestBrowserAccountDTOsExcludeStoreInternals(t *testing.T) {
	userJSON, err := json.Marshal(overviewUserDTOs([]store.User{{
		ID: 1, Username: "grace", PasswordHash: "secret hash", AuthSource: "password", IsAdmin: true,
	}}))
	if err != nil {
		t.Fatalf("encode overview user DTO: %v", err)
	}
	for _, field := range []string{"PasswordHash", "AuthSource", "CreatedAt", "OverviewPublic"} {
		if strings.Contains(string(userJSON), field) {
			t.Errorf("overview user response exposes store field %s: %s", field, userJSON)
		}
	}

	tokenJSON, err := json.Marshal(accountTokenDTOs([]store.APIToken{{ID: 2, UserID: 99, Name: "CLI", Scope: "ingest"}}))
	if err != nil {
		t.Fatalf("encode account token DTO: %v", err)
	}
	if strings.Contains(string(tokenJSON), "UserID") {
		t.Errorf("account token response exposes its store owner: %s", tokenJSON)
	}
}

func normalizeContractSchema(t *testing.T, schema any) any {
	t.Helper()
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("encode expected contract schema: %v", err)
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatalf("normalize expected contract schema: %v", err)
	}
	return normalized
}

func readContractDocument(t *testing.T) contractDocument {
	t.Helper()
	var document contractDocument
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatalf("decode embedded OpenAPI document: %v", err)
	}
	return document
}

func browserContractSchemas(t *testing.T) map[string]any {
	t.Helper()
	names := map[reflect.Type]string{
		reflect.TypeOf(appViewer{}):                  "Viewer",
		reflect.TypeOf(overviewUserDTO{}):            "OverviewUser",
		reflect.TypeOf(accountTokenDTO{}):            "AccountToken",
		reflect.TypeOf(oauthGrantDTO{}):              "OAuthGrant",
		reflect.TypeOf(accountInviteDTO{}):           "AccountInvite",
		reflect.TypeOf(reparseStatusResponse{}):      "ReparseStatusResponse",
		reflect.TypeOf(reparseGateResponse{}):        "ProjectionRebuild",
		reflect.TypeOf(overviewResponse{}):           "OverviewResponse",
		reflect.TypeOf(insightsResponse{}):           "InsightsResponse",
		reflect.TypeOf(projectsResponse{}):           "ProjectsResponse",
		reflect.TypeOf(projectResponse{}):            "ProjectResponse",
		reflect.TypeOf(sessionsResponse{}):           "SessionsResponse",
		reflect.TypeOf(sessionResponse{}):            "SessionResponse",
		reflect.TypeOf(transcriptResponse{}):         "TranscriptResponse",
		reflect.TypeOf(accountResponse{}):            "AccountResponse",
		reflect.TypeOf(guideResponse{}):              "GuideResponse",
		reflect.TypeOf(publicOverviewResponse{}):     "PublicOverviewResponse",
		reflect.TypeOf(publicProjectResponse{}):      "PublicProjectResponse",
		reflect.TypeOf(publicSessionResponse{}):      "PublicSessionResponse",
		reflect.TypeOf(oauthConsentResponse{}):       "OAuthConsentResponse",
		reflect.TypeOf(publicationResponse{}):        "PublicationResponse",
		reflect.TypeOf(sessionPublicationResponse{}): "SessionPublicationResponse",
		reflect.TypeOf(deletedSessionResponse{}):     "DeletedSessionResponse",
		reflect.TypeOf(revokedResponse{}):            "RevokedResponse",
		reflect.TypeOf(registeredUserResponse{}):     "RegisteredUserResponse",
		reflect.TypeOf(loginResponse{}):              "LoginResponse",
		reflect.TypeOf(statusResponse{}):             "StatusResponse",
		reflect.TypeOf(createdTokenResponse{}):       "CreatedTokenResponse",
		reflect.TypeOf(tokenListItem{}):              "TokenListItem",
		reflect.TypeOf(tokensResponse{}):             "TokensResponse",
		reflect.TypeOf(createdInviteResponse{}):      "CreatedInviteResponse",
	}
	for _, contract := range browserContracts {
		names[reflect.TypeOf(contract.response)] = contract.schemaName
	}

	builder := schemaBuilder{names: names, schemas: make(map[string]any), building: make(map[reflect.Type]bool)}
	for typ := range names {
		builder.component(typ)
	}
	return builder.schemas
}

type schemaBuilder struct {
	names    map[reflect.Type]string
	schemas  map[string]any
	building map[reflect.Type]bool
}

func (b *schemaBuilder) component(typ reflect.Type) {
	name := b.schemaName(typ)
	if _, ok := b.schemas[name]; ok || b.building[typ] {
		return
	}
	b.building[typ] = true
	b.schemas[name] = b.structSchema(typ)
	delete(b.building, typ)
}

func (b *schemaBuilder) schemaName(typ reflect.Type) string {
	if name, ok := b.names[typ]; ok {
		return name
	}
	name := typ.Name()
	if name == "" {
		panic("unnamed component type: " + typ.String())
	}
	b.names[typ] = name
	return name
}

func (b *schemaBuilder) structSchema(typ reflect.Type) map[string]any {
	properties := map[string]any{}
	required := []string{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		tag, options := parseJSONTag(field.Tag.Get("json"))
		if tag == "-" {
			continue
		}
		if field.Anonymous && tag == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			schema := b.structSchema(embedded)
			for name, value := range schema["properties"].(map[string]any) {
				properties[name] = value
			}
			required = append(required, schema["required"].([]string)...)
			continue
		}
		name := tag
		if name == "" {
			name = field.Name
		}
		properties[name] = b.fieldSchema(field.Type)
		if !options["omitempty"] {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	return map[string]any{
		"additionalProperties": false,
		"properties":           properties,
		"required":             required,
		"type":                 "object",
	}
}

func (b *schemaBuilder) fieldSchema(typ reflect.Type) any {
	if typ == reflect.TypeOf(projectionRebuildError("")) {
		return map[string]any{"const": string(projectionRebuildInProgress), "type": "string"}
	}
	if typ == reflect.TypeOf(projectionRebuildCode("")) {
		return map[string]any{"const": string(projectionRebuildCodeValue), "type": "string"}
	}
	if typ == reflect.TypeOf(time.Time{}) {
		return map[string]any{"format": "date-time", "type": "string"}
	}
	if typ == reflect.TypeOf(time.Duration(0)) {
		return map[string]any{"format": "int64", "type": "integer"}
	}
	switch typ.Kind() {
	case reflect.Pointer:
		return map[string]any{"anyOf": []any{b.fieldSchema(typ.Elem()), map[string]any{"type": "null"}}}
	case reflect.Struct:
		b.component(typ)
		return map[string]any{"$ref": "#/components/schemas/" + b.schemaName(typ)}
	case reflect.Slice, reflect.Array:
		array := map[string]any{"items": b.fieldSchema(typ.Elem()), "type": []any{"array", "null"}}
		if typ.Kind() == reflect.Array {
			array["maxItems"] = float64(typ.Len())
			array["minItems"] = float64(typ.Len())
			array["type"] = "array"
		}
		return array
	case reflect.Map:
		return map[string]any{"additionalProperties": b.fieldSchema(typ.Elem()), "type": []any{"object", "null"}}
	case reflect.Interface:
		return map[string]any{}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"format": "int64", "type": "integer"}
	default:
		panic("unsupported contract field type: " + typ.String())
	}
}

func parseJSONTag(raw string) (string, map[string]bool) {
	parts := strings.Split(raw, ",")
	options := make(map[string]bool, len(parts)-1)
	for _, option := range parts[1:] {
		options[option] = true
	}
	return parts[0], options
}
