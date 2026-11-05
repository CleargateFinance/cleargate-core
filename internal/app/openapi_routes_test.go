//go:build integration

package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ginParam is a path parameter as Gin spells it, ":id", which OpenAPI spells
// "{id}".
var ginParam = regexp.MustCompile(`:([A-Za-z]+)`)

// TestOpenAPI_DescribesEveryRouteAndNoOther: a route missing from the spec is
// one no generated client can call, and a path in the spec with no route is
// one every generated client calls into a 404.
func TestOpenAPI_DescribesEveryRouteAndNoOther(t *testing.T) {
	f := newAPIFixture(t)

	var served []string
	for _, r := range f.api.Engine.Routes() {
		served = append(served, r.Method+" "+ginParam.ReplaceAllString(r.Path, "{$1}"))
	}

	var described []string
	for path, item := range loadSpec(t).Paths.Map() {
		for method := range item.Operations() {
			described = append(described, method+" "+path)
		}
	}

	sort.Strings(served)
	sort.Strings(described)
	assert.Equal(t, served, described)
}

// contract validates responses against the spec. Built once: the router
// compiles every path.
var contract = sync.OnceValues(func() (routers.Router, error) {
	doc, err := spec()
	if err != nil {
		return nil, err
	}
	// The spec names the production host. Tests call httptest's, so match on
	// the path alone, from a copy, leaving the shared document untouched.
	local := *doc
	local.Servers = nil
	return legacy.NewRouter(&local)
})

// conform checks one response the API gave against the spec. Every request
// the integration tests send through serve and do passes through here, so a
// handler that changes a field's name or type, adds a status, or returns null
// where the spec promises a value fails the test that exercised it.
//
// A request to a path the spec does not describe is left alone. That is a
// test's own probe route, or an unknown path, and
// TestOpenAPI_DescribesEveryRouteAndNoOther is what catches a real route
// missing from the spec.
func (f *apiFixture) conform(req *http.Request, rec *httptest.ResponseRecorder) {
	f.t.Helper()
	router, err := contract()
	require.NoError(f.t, err)

	route, params, err := router.FindRoute(req)
	if err != nil {
		return
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
		},
		Status: rec.Code,
		Header: rec.Header(),
		Body:   io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
		Options: &openapi3filter.Options{
			IncludeResponseStatus: true,
			MultiError:            true,
		},
	}
	err = openapi3filter.ValidateResponse(context.Background(), in)
	assert.NoError(f.t, err, "%s %s answered %d against the spec: %s",
		req.Method, req.URL.Path, rec.Code, strings.TrimSpace(rec.Body.String()))
}
