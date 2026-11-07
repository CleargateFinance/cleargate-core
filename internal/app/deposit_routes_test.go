//go:build integration

package app

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/cdp"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
)

type depositAddressJSON struct {
	Address string `json:"address"`
	Network string `json:"network"`
	Token   string `json:"token"`
	Mint    string `json:"mint"`
}

// TestDepositAddressRoute_OneWalletForEveryMember: the first request creates
// the account's wallet at the custodian, and every member after it reads the
// same address without reaching the custodian again.
func TestDepositAddressRoute_OneWalletForEveryMember(t *testing.T) {
	f := newAPIFixture(t)
	path := "/v1/accounts/" + string(f.accountID) + "/deposit-address"

	first := f.do(http.MethodGet, path, "", credentials{sessionToken: f.session("owner@example.com", "owner", false)})
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	got := decode[depositAddressJSON](t, first.Body.Bytes())
	assert.NotEmpty(t, got.Address)
	assert.Equal(t, devnetRail.Network, got.Network)
	assert.Equal(t, "USDC", got.Token)
	assert.NotEmpty(t, got.Mint)

	viewer := f.do(http.MethodGet, path, "", credentials{sessionToken: f.session("viewer@example.com", "viewer", false)})
	require.Equal(t, http.StatusOK, viewer.Code, viewer.Body.String())
	assert.Equal(t, got.Address, decode[depositAddressJSON](t, viewer.Body.Bytes()).Address)

	assert.Equal(t, 1, f.cdp.Creates())
}

// TestDepositAddressRoute_ReachesNoCustodianForAStranger: somebody outside the
// account gets the answer an unknown account gets, and creates no wallet.
func TestDepositAddressRoute_ReachesNoCustodianForAStranger(t *testing.T) {
	f := newAPIFixture(t)
	other := &apiFixture{t: t, db: f.db, api: f.api}
	other.seedAgent()
	stranger := other.session("stranger@example.com", "owner", false)

	for name, accountID := range map[string]string{
		"somebody else's account": string(f.accountID),
		"unknown account":         "00000000-0000-0000-0000-000000000000",
		"not a uuid":              "nope",
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.do(http.MethodGet, "/v1/accounts/"+accountID+"/deposit-address", "", credentials{sessionToken: stranger})
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}
	assert.Zero(t, f.cdp.Creates())
}

func TestDepositAddressRoute_IsNotOnTheAgentSurface(t *testing.T) {
	f := newAPIFixture(t)

	rec := f.do(http.MethodGet, "/v1/accounts/"+string(f.accountID)+"/deposit-address", "", credentials{agentKey: f.agentKey})

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Zero(t, f.cdp.Creates())
}

// TestBuildAPI_RefusesNoCustodian: an API that cannot sign must say so at
// boot, not at an account's first deposit.
func TestBuildAPI_RefusesNoCustodian(t *testing.T) {
	cfg := *newAPIFixture(t).cfg
	cfg.CDP = config.CDP{}

	_, _, err := BuildAPI(context.Background(), &cfg, slog.Default())

	assert.ErrorIs(t, err, cdp.ErrMissingCredentials)
}
