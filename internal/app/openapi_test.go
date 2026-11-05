package app

import (
	"context"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/modules/authz"
)

// These tests hold api/openapi.yaml, the contract the SDK, MCP server and
// console generate from, to the server. The integration tests go further:
// every route is in it, and every response they receive validates against it
// (openapi_routes_test.go).

const specPath = "../../api/openapi.yaml"

var spec = sync.OnceValues(func() (*openapi3.T, error) {
	return openapi3.NewLoader().LoadFromFile(specPath)
})

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := spec()
	require.NoError(t, err)
	return doc
}

func TestOpenAPI_IsAValidDocument(t *testing.T) {
	require.NoError(t, loadSpec(t).Validate(context.Background()))
}

// TestOpenAPI_ReasonCodesAreTheEnginesInOrder: the codes are a public
// contract and so is their order, since the first failing check wins and an
// agent recovers by the code it gets (Task 4.1). The decision table's CHECK
// list is held to the same function (TestDecision_ReasonCodesMatchTheEngine).
func TestOpenAPI_ReasonCodesAreTheEnginesInOrder(t *testing.T) {
	var published []string
	for _, v := range loadSpec(t).Components.Schemas["ReasonCode"].Value.Enum {
		published = append(published, v.(string))
	}

	var engine []string
	for _, c := range authz.ReasonCodes() {
		engine = append(engine, string(c))
	}
	assert.Equal(t, engine, published)
}
