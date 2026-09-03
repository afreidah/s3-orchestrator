// -------------------------------------------------------------------------------
// OpenAPI - Document Assembly
//
// Author: Alex Freidah
//
// Turns a list of route descriptors into an OpenAPI 3.1 document. Callers hand
// over what their route table already knows - method, path, summary, the
// parameters the handler reads, and the types it exchanges - and every schema
// is reflected from those types rather than written by hand. Output is
// deterministic so a regenerated document is byte-identical when nothing
// changed, which is what lets a test diff it against the committed copy.
//
// This package is generator-only: importing it from server code would link a
// documentation tool into the daemon, so it is imported from tests alone.
// -------------------------------------------------------------------------------

package openapi

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parameter locations.
const (
	InQuery = "query"
	InPath  = "path"
)

// Parameter and schema primitive types, shared by the descriptors callers
// write and the schemas the reflector emits.
const (
	TypeString  = "string"
	TypeInteger = "integer"
	TypeBoolean = "boolean"
	typeObject  = "object"
	typeArray   = "array"
	typeNumber  = "number"
)

// JSON Schema keywords, named so a typo in one of the hand-built schemas
// cannot silently emit a key nothing reads.
const (
	keyType   = "type"
	keyFormat = "format"
)

// jsonMediaType is the default success media type.
const jsonMediaType = "application/json"

// Info is the document's identifying header.
type Info struct {
	Title       string
	Version     string
	Description string
}

// Param is one query or path parameter a handler reads.
type Param struct {
	Name        string
	In          string
	Description string
	Required    bool
	Type        string
}

// Route is one endpoint. Request, Stream and Alt are nil for the routes that
// do not use them; ResponseType is set only when the route answers with
// something other than JSON.
type Route struct {
	Method          string
	Path            string
	Summary         string
	Params          []Param
	Request         any
	Response        any
	Stream          any
	Alt             any
	ResponseType    string
	RequestType     string // media type of a non-JSON body; takes precedence over Request
	StreamMediaType string // required when Stream is set
}

// SecurityScheme describes how callers authenticate.
type SecurityScheme struct {
	Name        string
	HeaderName  string
	Description string
}

// Generate renders the document as YAML.
func Generate(info Info, sec SecurityScheme, routes []Route) ([]byte, error) {
	doc := document{
		OpenAPI: "3.1.0",
		Info:    docInfo(info),
		Paths:   map[string]map[string]operation{},
		Components: components{
			Schemas: schemaSet{},
			SecuritySchemes: map[string]securityScheme{
				sec.Name: {
					Type:        "apiKey",
					In:          "header",
					ParamName:   sec.HeaderName,
					Description: sec.Description,
				},
			},
		},
		Security: []map[string][]string{{sec.Name: {}}},
	}

	for i := range routes {
		if err := addRoute(&doc, &routes[i]); err != nil {
			return nil, fmt.Errorf("%s %s: %w", routes[i].Method, routes[i].Path, err)
		}
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal document: %w", err)
	}
	return out, nil
}

// addRoute folds one route into the document, registering any schemas it
// introduces.
func addRoute(doc *document, rt *Route) error {
	op := operation{
		Summary:     rt.Summary,
		OperationID: operationID(rt.Method, rt.Path),
		Responses:   map[string]response{},
	}

	for _, p := range rt.Params {
		op.Parameters = append(op.Parameters, parameter{
			Name:        p.Name,
			In:          p.In,
			Description: p.Description,
			Required:    p.Required || p.In == InPath,
			Schema:      map[string]any{keyType: p.Type},
		})
	}

	switch {
	case rt.RequestType != "":
		// A raw body has no JSON Schema to describe; the media type is the
		// whole contract.
		op.RequestBody = &requestBody{
			Required: true,
			Content:  map[string]mediaType{rt.RequestType: {Schema: map[string]any{keyType: TypeString, keyFormat: "binary"}}},
		}

	case rt.Request != nil:
		schema, err := schemaFor(rt.Request, doc.Components.Schemas)
		if err != nil {
			return fmt.Errorf("request body: %w", err)
		}
		op.RequestBody = &requestBody{
			Required: true,
			Content:  map[string]mediaType{jsonMediaType: {Schema: schema}},
		}
	}

	success, err := successResponse(rt, doc.Components.Schemas)
	if err != nil {
		return err
	}
	op.Responses["200"] = success
	// Every route is mounted behind the same token middleware, so every
	// route can answer 401.
	op.Responses["401"] = response{Description: "Missing or invalid admin token"}

	path := openAPIPath(rt.Path)
	if doc.Paths[path] == nil {
		doc.Paths[path] = map[string]operation{}
	}
	doc.Paths[path][strings.ToLower(rt.Method)] = op
	return nil
}

// successResponse builds the 200 entry, describing every media type the route
// can answer with.
func successResponse(rt *Route, set schemaSet) (response, error) {
	res := response{Description: "Success", Content: map[string]mediaType{}}

	switch {
	case rt.ResponseType != "":
		// A non-JSON body has no JSON Schema to describe; the media type is
		// the whole contract.
		res.Content[rt.ResponseType] = mediaType{Schema: map[string]any{keyType: TypeString, keyFormat: "binary"}}

	default:
		schema, err := schemaFor(rt.Response, set)
		if err != nil {
			return response{}, fmt.Errorf("response body: %w", err)
		}
		if rt.Alt != nil {
			alt, err := schemaFor(rt.Alt, set)
			if err != nil {
				return response{}, fmt.Errorf("alternate response body: %w", err)
			}
			schema = map[string]any{"oneOf": []any{schema, alt}}
		}
		res.Content[jsonMediaType] = mediaType{Schema: schema}
	}

	if rt.Stream != nil {
		stream, err := schemaFor(rt.Stream, set)
		if err != nil {
			return response{}, fmt.Errorf("stream body: %w", err)
		}
		res.Content[rt.StreamMediaType] = mediaType{
			Schema:      stream,
			Description: "One JSON object per line while the operation runs",
		}
	}
	return res, nil
}

// pathParamPattern matches a Go 1.22 mux wildcard, including the trailing
// "..." form used for keys that contain slashes.
var pathParamPattern = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\.\.\.\}`)

// openAPIPath converts a net/http mux pattern into an OpenAPI path. The only
// difference is the greedy wildcard, which OpenAPI writes as an ordinary
// parameter.
func openAPIPath(pattern string) string {
	return pathParamPattern.ReplaceAllString(pattern, "{$1}")
}

// nonAlnum matches the separators in a path so operationID can split on them.
var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// operationID derives a stable, unique identifier for a route, which client
// generators use to name the method they emit.
func operationID(method, path string) string {
	parts := nonAlnum.Split(strings.TrimPrefix(path, "/admin/api/"), -1)
	var id strings.Builder
	id.WriteString(strings.ToLower(method))
	for _, p := range parts {
		if p == "" {
			continue
		}
		id.WriteString(strings.ToUpper(p[:1]))
		id.WriteString(p[1:])
	}
	return id.String()
}

// -------------------------------------------------------------------------
// DOCUMENT SHAPE
// -------------------------------------------------------------------------

// The document is modelled with structs so field order in the emitted YAML is
// the conventional one; maps are used only where the keys are data (paths,
// schema names), and yaml.v3 sorts those, keeping output deterministic.

type document struct {
	OpenAPI    string                          `yaml:"openapi"`
	Info       docInfo                         `yaml:"info"`
	Security   []map[string][]string           `yaml:"security"`
	Paths      map[string]map[string]operation `yaml:"paths"`
	Components components                      `yaml:"components"`
}

type docInfo struct {
	Title       string `yaml:"title"`
	Version     string `yaml:"version"`
	Description string `yaml:"description,omitempty"`
}

type components struct {
	Schemas         schemaSet                 `yaml:"schemas"`
	SecuritySchemes map[string]securityScheme `yaml:"securitySchemes"`
}

type securityScheme struct {
	Type        string `yaml:"type"`
	In          string `yaml:"in"`
	ParamName   string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}

type operation struct {
	Summary     string              `yaml:"summary"`
	OperationID string              `yaml:"operationId"`
	Parameters  []parameter         `yaml:"parameters,omitempty"`
	RequestBody *requestBody        `yaml:"requestBody,omitempty"`
	Responses   map[string]response `yaml:"responses"`
}

type parameter struct {
	Name        string         `yaml:"name"`
	In          string         `yaml:"in"`
	Description string         `yaml:"description,omitempty"`
	Required    bool           `yaml:"required"`
	Schema      map[string]any `yaml:"schema"`
}

type requestBody struct {
	Required bool                 `yaml:"required"`
	Content  map[string]mediaType `yaml:"content"`
}

type response struct {
	Description string               `yaml:"description"`
	Content     map[string]mediaType `yaml:"content,omitempty"`
}

type mediaType struct {
	Description string         `yaml:"description,omitempty"`
	Schema      map[string]any `yaml:"schema"`
}
