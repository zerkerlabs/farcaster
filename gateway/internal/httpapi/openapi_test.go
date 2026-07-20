package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/credential"
	"github.com/zerkerlabs/farcaster/gateway/internal/httpapi"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
	"github.com/zerkerlabs/farcaster/gateway/internal/kms"
	"github.com/zerkerlabs/farcaster/gateway/internal/policy"
	"github.com/zerkerlabs/farcaster/gateway/internal/settlement"
)

// openapiPath is gateway/openapi.yaml, the canonical /v1 REST contract (spec
// 0008 T5). Tests run with the package directory as the working directory, so
// the path is relative to gateway/internal/httpapi/.
const openapiPath = "../../openapi.yaml"

// registeredRoutes mirrors Handler.RegisterRoutes in handler.go exactly — the
// full set of "METHOD /path" patterns registered once every optional
// dependency (credentials, proxy, invocations, settlement) is wired. Keep this
// in lockstep with RegisterRoutes: this is the "validation hook keeping
// openapi.yaml honest against the handlers" the ticket asks for (spec 0008
// T5). A route added to one and not the other fails TestOpenAPIMatchesRoutes.
var registeredRoutes = []string{
	"POST /v1/agents",
	"GET /v1/agents",
	"GET /v1/agents/{id}",
	"PATCH /v1/agents/{id}",
	"DELETE /v1/agents/{id}",

	"POST /v1/credentials",
	"GET /v1/credentials",
	"GET /v1/credentials/{id}",
	"PUT /v1/credentials/{id}",
	"DELETE /v1/credentials/{id}",

	"GET /v1/invocations",
	"GET /v1/invocations/{id}",
	"GET /v1/analytics",

	"PATCH /v1/settlement/config",
	"GET /v1/settlement/config",

	"GET /v1/policy",
	"PUT /v1/policy",
	"GET /v1/policy/decisions",

	"POST /v1/proxy/{id}",
	"POST /v1/proxy/{id}/stream",
	"GET /v1/proxy/{id}/invocations/{inv_id}",
}

// operationalRoutes are registered directly by internal/server (not by
// Handler.RegisterRoutes) and are exempt from authentication.
var operationalRoutes = []string{
	"GET /healthz",
	"GET /version",
}

// fullyWiredHandler returns a Handler with every optional dependency attached,
// so RegisterRoutes mounts the complete /v1 surface (agents, credentials,
// proxy, invocations, analytics, settlement/config).
func fullyWiredHandler(t *testing.T) *httpapi.Handler {
	t.Helper()

	credStore := credential.NewMemoryStore()
	kekStore := credential.NewMemoryKEKStore()
	provider, err := kms.NewLocalProvider()
	if err != nil {
		t.Fatalf("new kms provider: %v", err)
	}
	credSvc := credential.NewService(credStore, kekStore, provider, credential.StubVaultResolver{})

	return httpapi.NewHandler(agent.NewMemoryStore(), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentials(credSvc).
		WithProxy(&mockForwarder{}, invocation.NewMemoryStore()).
		WithSettlement(settlement.NewMemoryStore()).
		WithPolicy(policy.NewMemoryStore()).
		WithPolicyDecisions(policy.NewMemoryDecisionStore())
}

// TestOpenAPIDocumentIsValid loads and validates gateway/openapi.yaml against
// the OpenAPI 3 specification (required fields, resolvable $refs, well-formed
// schemas). This is the "it validates/lints" acceptance bullet (spec 0008 T5).
func TestOpenAPIDocumentIsValid(t *testing.T) {
	t.Parallel()

	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true

	doc, err := loader.LoadFromFile(openapiPath)
	if err != nil {
		t.Fatalf("load %s: %v", openapiPath, err)
	}

	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("validate %s: %v", openapiPath, err)
	}
}

// TestOpenAPIMatchesRoutes cross-checks gateway/openapi.yaml against the
// gateway's actual registered mux routes in both directions: every route
// RegisterRoutes mounts must appear in the spec (with the same path/method),
// and every path+method in the spec must resolve to a live handler. This is
// the "matches the handlers' actual request/response shapes" acceptance
// bullet (spec 0008 T5) — a real, if partial, drift guard: it cannot detect a
// route whose *shape* changed without a path/method change, but it does catch
// a route added, removed, or renamed in only one of the two places.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	t.Parallel()

	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromFile(openapiPath)
	if err != nil {
		t.Fatalf("load %s: %v", openapiPath, err)
	}

	specRoutes := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			specRoutes[method+" "+path] = true
		}
	}

	for _, route := range operationalRoutes {
		if !specRoutes[route] {
			t.Errorf("openapi.yaml is missing operational route %q", route)
		}
		delete(specRoutes, route)
	}

	mux := http.NewServeMux()
	fullyWiredHandler(t).RegisterRoutes(mux)

	for _, route := range registeredRoutes {
		if !specRoutes[route] {
			t.Errorf("openapi.yaml is missing route %q (registered by Handler.RegisterRoutes)", route)
		}
		delete(specRoutes, route)

		method, path, ok := splitRoute(route)
		if !ok {
			t.Fatalf("malformed route in registeredRoutes: %q", route)
		}
		req := httptest.NewRequest(method, examplePath(path), nil)
		_, pattern := mux.Handler(req)
		if pattern != route {
			t.Errorf("mux does not register %q (got pattern %q) — registeredRoutes is out of sync with Handler.RegisterRoutes", route, pattern)
		}
	}

	for extra := range specRoutes {
		t.Errorf("openapi.yaml documents route %q that Handler.RegisterRoutes does not register", extra)
	}
}

// splitRoute splits a "METHOD /path" route into its method and path.
func splitRoute(route string) (method, path string, ok bool) {
	for i := 0; i < len(route); i++ {
		if route[i] == ' ' {
			return route[:i], route[i+1:], true
		}
	}
	return "", "", false
}

// examplePath substitutes OpenAPI-style {name} path parameters with a dummy
// value so the request can be routed by net/http's ServeMux, which uses the
// identical {name} wildcard syntax.
func examplePath(path string) string {
	out := make([]byte, 0, len(path))
	inParam := false
	for i := 0; i < len(path); i++ {
		switch {
		case path[i] == '{':
			inParam = true
			out = append(out, 'x')
		case path[i] == '}':
			inParam = false
		case !inParam:
			out = append(out, path[i])
		}
	}
	return string(out)
}
