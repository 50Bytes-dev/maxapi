package main

import (
	"bufio"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// specPath is the OpenAPI schema the gateway serves at /api/spec.yml.
const specPath = "static/api/spec.yml"

// specOperations reads the operations of the schema's paths: section as
// "METHOD /path" keys. The schema is block-style YAML: a path key is
// indented by two spaces, its methods by four.
func specOperations(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(specPath)
	if err != nil {
		t.Fatalf("open the schema: %v", err)
	}
	defer f.Close()

	methods := []string{"get", "put", "post", "delete", "patch", "head", "options", "trace"}
	var ops []string
	inPaths := false
	path := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " ")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			inPaths = line == "paths:"
			path = ""
			continue
		}
		if !inPaths {
			continue
		}
		switch {
		case strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":"):
			path = strings.Trim(strings.TrimSuffix(strings.TrimSpace(line), ":"), `"'`)
		case strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") && path != "":
			key := strings.TrimSuffix(strings.TrimSpace(line), ":")
			if slices.Contains(methods, key) {
				ops = append(ops, strings.ToUpper(key)+" "+path)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	return ops
}

// routerOperations lists the gateway's routes as "METHOD /path" keys. The
// static file server (PathPrefix "/", no methods) is no API operation and is
// left out with every other route without methods.
func routerOperations(t *testing.T, s *server) []string {
	t.Helper()
	var ops []string
	err := s.router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tmpl, err := route.GetPathTemplate()
		if err != nil {
			return nil // a route without a path, such as a matcher-only one
		}
		methods, err := route.GetMethods()
		if err != nil || len(methods) == 0 {
			return nil
		}
		for _, m := range methods {
			ops = append(ops, m+" "+tmpl)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the router: %v", err)
	}
	return ops
}

// TestSpecMatchesRoutes keeps static/api/spec.yml honest: it describes
// exactly the operations the router serves, no more and no fewer.
func TestSpecMatchesRoutes(t *testing.T) {
	s := newTestServer(t)
	s.router = mux.NewRouter()
	s.routes()
	routes := routerOperations(t, s)
	spec := specOperations(t)
	if len(routes) == 0 || len(spec) == 0 {
		t.Fatalf("found %d routes and %d schema operations", len(routes), len(spec))
	}

	inSpec := map[string]int{}
	for _, op := range spec {
		inSpec[op]++
	}
	inRoutes := map[string]bool{}
	for _, op := range routes {
		inRoutes[op] = true
		if inSpec[op] == 0 {
			t.Errorf("%s is served but missing from %s", op, specPath)
		}
	}
	for op, n := range inSpec {
		if !inRoutes[op] {
			t.Errorf("%s is in %s but not served", op, specPath)
		}
		if n > 1 {
			t.Errorf("%s is in %s %d times", op, specPath, n)
		}
	}
}
