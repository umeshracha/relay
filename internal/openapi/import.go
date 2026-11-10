package openapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"api-telemetry-gateway/internal/routing"
	"gopkg.in/yaml.v3"
)

const MaxDocument = 1 << 20

var supportedVersion = regexp.MustCompile(`^3\.[01]\.[0-9]+$`)

func Routes(reader io.Reader) ([]routing.Route, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxDocument+1))
	if err != nil || len(data) > MaxDocument {
		return nil, errors.New("could not read an OpenAPI document of up to 1 MiB")
	}
	var document struct {
		Version string                          `yaml:"openapi"`
		Paths   map[string]map[string]yaml.Node `yaml:"paths"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("invalid OpenAPI JSON or YAML")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("upload one OpenAPI document per file")
	}
	if !supportedVersion.MatchString(document.Version) {
		return nil, errors.New("OpenAPI 3.0 and 3.1 documents are supported")
	}
	var routes []routing.Route
	seen := make(map[string]bool)
	for path, operations := range document.Paths {
		if strings.HasPrefix(path, "x-") {
			continue
		}
		parts, ok := routing.Segments(path)
		if !ok {
			return nil, fmt.Errorf("unsupported path %q", path)
		}
		for i, part := range parts {
			if routing.Parameter(part) {
				parts[i] = "{}"
			} else if strings.ContainsAny(part, "{}") {
				return nil, fmt.Errorf("path parameters must occupy a full segment in %q", path)
			}
		}
		if _, ref := operations["$ref"]; ref {
			return nil, fmt.Errorf("path references must be resolved before importing %q", path)
		}
		for _, method := range []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"} {
			operation, ok := operations[method]
			if !ok {
				continue
			}
			if operation.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("invalid operation %s %q", method, path)
			}
			key := method + "/" + strings.Join(parts, "/")
			if seen[key] {
				return nil, fmt.Errorf("conflicting operation %s %q", method, path)
			}
			seen[key] = true
			routes = append(routes, routing.Route{Method: strings.ToUpper(method), Path: path})
			if len(routes) > 500 {
				return nil, errors.New("an import can contain up to 500 routes")
			}
		}
	}
	if len(routes) == 0 {
		return nil, errors.New("the document has no supported routes")
	}
	slices.SortFunc(routes, func(a, b routing.Route) int {
		if byPath := strings.Compare(a.Path, b.Path); byPath != 0 {
			return byPath
		}
		return strings.Compare(a.Method, b.Method)
	})
	return routes, nil
}
