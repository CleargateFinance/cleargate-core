// Command merchant is the demo's seller: Atlas Data, a market-data API that
// charges per request in devnet USDC over x402.
//
// It is an ordinary x402 resource server built with the x402 Go SDK. It knows
// nothing about Cleargate: it answers an unpaid request with 402 and a
// PAYMENT-REQUIRED header, and hands a paid one to the public x402 facilitator,
// which checks the transaction, pays its fee, submits it to Solana and waits
// for it to land. Only then does the handler run and the data go out.
//
//	go run ./merchant keygen   # once: create the merchant's wallet
//	go run ./merchant          # serve on :4021
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	sol "github.com/gagliardetto/solana-go"
	"github.com/gin-gonic/gin"
	x402 "github.com/x402-foundation/x402/go"
	x402http "github.com/x402-foundation/x402/go/http"
	ginmw "github.com/x402-foundation/x402/go/http/gin"
	svm "github.com/x402-foundation/x402/go/mechanisms/svm/exact/server"

	"github.com/CleargateFinance/cleargate-core/demo/internal/devnet"
)

const (
	// network and usdcMint are Solana devnet and Circle's devnet USDC, the
	// pair Cleargate's devnet configuration pays in. A merchant offering any
	// other pair would get "no usable option" from Cleargate.
	network  = devnet.Network
	usdcMint = devnet.USDCMint
)

// wallet is the merchant's state file, written by keygen.
type wallet struct {
	// Address receives the payments.
	Address string `json:"address"`
	// SecretKey lets you move what the demo earned. The merchant never signs:
	// receiving needs no signature and the facilitator pays every fee.
	SecretKey string `json:"secret_key"`
	// Partner is a second payee with no key at all. One route pays it, so the
	// demo can show a mandate refusing a payee it does not list.
	Partner string `json:"partner"`
}

func main() {
	stateDir := envOr("DEMO_STATE_DIR", ".state")
	walletFile := filepath.Join(stateDir, "merchant.json")

	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		if err := keygen(walletFile); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := serve(walletFile); err != nil {
		log.Fatal(err)
	}
}

func keygen(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; delete it first to make a new wallet", path)
	}
	merchant, err := sol.NewRandomPrivateKey()
	if err != nil {
		return err
	}
	partner, err := sol.NewRandomPrivateKey()
	if err != nil {
		return err
	}
	w := wallet{Address: merchant.PublicKey().String(), SecretKey: merchant.String(), Partner: partner.PublicKey().String()}
	if err := writeJSON(path, w); err != nil {
		return err
	}
	fmt.Printf("Merchant wallet: %s\nPartner payee:   %s\nSaved to %s\n\n", w.Address, w.Partner, path)
	fmt.Println("This wallet needs devnet USDC once, so its USDC token account exists: an x402 payment")
	fmt.Println("cannot create it. `go run ./console fund` sends it 1 USDC from CDP's faucet, or send")
	fmt.Println("some yourself from https://faucet.circle.com (Solana Devnet).")
	return nil
}

func serve(walletFile string) error {
	w, err := loadWallet(walletFile)
	if err != nil {
		return err
	}
	addr := envOr("MERCHANT_ADDR", ":4021")
	facilitatorURL := envOr("X402_FACILITATOR_URL", x402http.DefaultFacilitatorURL)
	rpcURL := envOr("SOLANA_RPC_URL", "https://api.devnet.solana.com")

	if err := checkTokenAccount(rpcURL, w.Address); err != nil {
		log.Printf("WARNING: %v", err)
	}

	routes := x402http.RoutesConfig{
		"GET /v1/quotes/*":        paid(w.Address, "10000", "One live quote for a ticker"),
		"GET /v1/reports/*":       paid(w.Address, "500000", "An analyst report on one sector"),
		"GET /v1/datasets/full":   paid(w.Address, "5000000", "Ten years of daily prices, every ticker"),
		"GET /v1/partner/signals": paid(w.Partner, "50000", "Trading signals, resold from a partner"),
	}

	facilitator := x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: facilitatorURL})
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	r.Use(ginmw.PaymentMiddlewareFromConfig(routes,
		ginmw.WithFacilitatorClient(facilitator),
		// The exact network, not "solana:*": this SDK version builds a 402 by
		// looking the network up exactly, so a wildcard registration never matches.
		ginmw.WithScheme(network, svm.NewExactSvmScheme()),
		// The facilitator waits for the transaction to land before answering,
		// which on devnet can take longer than the SDK's default.
		ginmw.WithTimeout(90*time.Second),
		ginmw.WithSettlementHandler(func(_ *gin.Context, s *x402.SettleResponse) {
			log.Printf("paid: %s atomic USDC from %s, tx https://explorer.solana.com/tx/%s?cluster=devnet",
				s.Amount, s.Payer, s.Transaction)
		}),
	))

	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"merchant": "Atlas Data",
			"network":  network,
			"pay_to":   w.Address,
			"catalog": []gin.H{
				{"path": "/v1/quotes/{ticker}", "price_usdc": "0.01"},
				{"path": "/v1/reports/{sector}", "price_usdc": "0.50"},
				{"path": "/v1/datasets/full", "price_usdc": "5.00"},
				{"path": "/v1/partner/signals", "price_usdc": "0.05", "pay_to": w.Partner},
			},
		})
	})
	r.GET("/v1/quotes/:ticker", quote)
	r.GET("/v1/reports/:sector", report)
	r.GET("/v1/datasets/full", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"rows": 2_520_000, "download": "https://atlas.example/datasets/full.parquet"})
	})
	r.GET("/v1/partner/signals", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"signals": []string{"SOL long", "ETH flat"}})
	})

	log.Printf("Atlas Data on %s, paid to %s, facilitator %s", addr, w.Address, facilitatorURL)
	srv := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

// paid is a route priced in atomic USDC. Giving the SDK the atomic amount and
// the mint, rather than "$0.01", keeps the price exact and pins the token.
func paid(payTo, atomic, description string) x402http.RouteConfig {
	return x402http.RouteConfig{
		Accepts: x402http.PaymentOptions{{
			Scheme:            "exact",
			PayTo:             payTo,
			Price:             map[string]interface{}{"amount": atomic, "asset": usdcMint},
			Network:           network,
			MaxTimeoutSeconds: 60,
		}},
		Description: description,
		MimeType:    "application/json",
	}
}

func quote(c *gin.Context) {
	ticker := strings.ToUpper(c.Param("ticker"))
	// Made-up but stable numbers, so a recording shows the same thing twice.
	price := 100 + float64(len(ticker))*7.25
	c.JSON(http.StatusOK, gin.H{"ticker": ticker, "price_usd": fmt.Sprintf("%.2f", price), "as_of": time.Now().UTC().Format(time.RFC3339)})
}

func report(c *gin.Context) {
	sector := c.Param("sector")
	c.JSON(http.StatusOK, gin.H{
		"sector":  sector,
		"summary": fmt.Sprintf("%s: margins stable, capex rising, two names upgraded this quarter.", sector),
		"pages":   14,
	})
}

// checkTokenAccount warns when the merchant's USDC token account is missing.
// An x402 `exact` transaction carries only the transfer, never an instruction
// creating the receiving account, so every payment to such a wallet fails.
func checkTokenAccount(rpcURL, owner string) error {
	ready, err := devnet.HasUSDCAccount(context.Background(), rpcURL, owner)
	if err != nil {
		return fmt.Errorf("could not check the merchant's USDC account: %w", err)
	}
	if !ready {
		return fmt.Errorf("%s has no USDC token account yet, so payments to it will fail: "+
			"run `go run ./console fund`, or send it devnet USDC once from https://faucet.circle.com", owner)
	}
	return nil
}

func loadWallet(path string) (wallet, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the demo's own state file
	if errors.Is(err, os.ErrNotExist) {
		return wallet{}, fmt.Errorf("no merchant wallet at %s: run `go run ./merchant keygen` first", path)
	}
	if err != nil {
		return wallet{}, err
	}
	var w wallet
	if err := json.Unmarshal(raw, &w); err != nil {
		return wallet{}, fmt.Errorf("%s: %w", path, err)
	}
	return w, nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
