package app

import (
	"context"
	"errors"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana"
	"github.com/CleargateFinance/cleargate-core/internal/modules/account"
	"github.com/CleargateFinance/cleargate-core/internal/modules/authz"
	"github.com/CleargateFinance/cleargate-core/internal/modules/counterparty"
	"github.com/CleargateFinance/cleargate-core/internal/modules/decisionlog"
	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
	"github.com/CleargateFinance/cleargate-core/internal/modules/mandate"
	"github.com/CleargateFinance/cleargate-core/internal/modules/payment"
	"github.com/CleargateFinance/cleargate-core/internal/modules/settlement"
	"github.com/CleargateFinance/cleargate-core/internal/modules/summary"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Adapters translate one module's service into the port another declares.
// They live here because this is the only package allowed to know both sides,
// and they hold no logic of their own beyond the translation.

// MandateAccounts satisfies mandate.Accounts with the account module. Wired
// into BuildAPI with the console's mandate routes (Task 6.2).
type MandateAccounts struct {
	Accounts *account.Service
}

var _ mandate.Accounts = MandateAccounts{}

// Agent implements mandate.Accounts.
func (a MandateAccounts) Agent(ctx context.Context, agentID id.AgentID) (mandate.Agent, error) {
	agent, err := a.Accounts.Agent(ctx, agentID)
	if err != nil {
		return mandate.Agent{}, err
	}
	return mandate.Agent{ID: agent.ID, AccountID: agent.AccountID, Active: agent.Active()}, nil
}

// RequireMandateIssuer implements mandate.Accounts.
//
// The session is rebuilt from the two fields the actor carries, which are the
// only ones the role and step-up checks read. The handler copies them from a
// session account.ValidateSession returned on the same request.
func (a MandateAccounts) RequireMandateIssuer(ctx context.Context, actor mandate.Actor, accountID id.AccountID) error {
	return a.Accounts.RequireMandateIssuer(ctx,
		account.Session{UserID: actor.UserID, MFASatisfiedAt: actor.MFASatisfiedAt},
		accountID)
}

// PaymentAccounts satisfies payment.Accounts with the account module.
type PaymentAccounts struct {
	Accounts *account.Service
}

var _ payment.Accounts = PaymentAccounts{}

// Agent implements payment.Accounts.
func (a PaymentAccounts) Agent(ctx context.Context, agentID id.AgentID) (payment.Agent, error) {
	agent, err := a.Accounts.Agent(ctx, agentID)
	if err != nil {
		return payment.Agent{}, err
	}
	return payment.Agent{ID: agent.ID, AccountID: agent.AccountID, Active: agent.Active()}, nil
}

// SpendingBlocked implements payment.Accounts.
func (a PaymentAccounts) SpendingBlocked(ctx context.Context, accountID id.AccountID) (bool, error) {
	return a.Accounts.SpendingBlocked(ctx, accountID)
}

// HoldSpending implements payment.Accounts.
func (a PaymentAccounts) HoldSpending(ctx context.Context, accountID id.AccountID, agentID id.AgentID) (bool, error) {
	return a.Accounts.HoldSpending(ctx, accountID, agentID)
}

// PaymentMandates satisfies payment.Mandates with the mandate module.
type PaymentMandates struct {
	Mandates *mandate.Service
}

var _ payment.Mandates = PaymentMandates{}

// Current implements payment.Mandates. The mandate module reports having none
// as an error, since for its own callers it is one; for the payment path it is
// the no_mandate input, so it becomes nil here.
func (m PaymentMandates) Current(ctx context.Context, agentID id.AgentID) (*payment.Mandate, error) {
	current, err := m.Mandates.Current(ctx, agentID)
	if errors.Is(err, mandate.ErrNoMandate) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := current.Rules
	return &payment.Mandate{
		ID:      current.ID,
		Version: current.Version,
		Digest:  current.Digest,
		Rules: authz.Rules{
			Asset:       r.Asset,
			MaxPerTx:    r.MaxPerTx,
			MaxPerDay:   r.MaxPerDay,
			MaxPerMonth: r.MaxPerMonth,
			Categories:  r.Categories,
			PayTo:       r.PayTo,
			ExpiresAt:   r.ExpiresAt,
		},
	}, nil
}

// HoldCurrent implements payment.Mandates.
func (m PaymentMandates) HoldCurrent(ctx context.Context, agentID id.AgentID, mandateID id.MandateID) (bool, error) {
	return m.Mandates.HoldCurrent(ctx, agentID, mandateID)
}

// PaymentCounterparties satisfies payment.Counterparties with the
// counterparty module.
type PaymentCounterparties struct {
	Counterparties *counterparty.Service
}

var _ payment.Counterparties = PaymentCounterparties{}

// Resolve implements payment.Counterparties.
func (c PaymentCounterparties) Resolve(ctx context.Context, p payment.Payee) (*id.CounterpartyID, error) {
	return c.Counterparties.Resolve(ctx, counterparty.Observation{
		ResourceURL: p.ResourceURL, Network: p.Network, PayTo: p.PayTo,
	})
}

// PaymentLedger satisfies payment.Ledger with the ledger module.
type PaymentLedger struct {
	Ledger *ledger.Service
}

var _ payment.Ledger = PaymentLedger{}

// Balance implements payment.Ledger.
func (l PaymentLedger) Balance(ctx context.Context, accountID id.AccountID, asset currency.Asset) (currency.Amount, error) {
	return l.Ledger.AccountBalance(ctx, accountID, asset)
}

// Spent implements payment.Ledger.
func (l PaymentLedger) Spent(
	ctx context.Context, agentID id.AgentID, asset currency.Asset, at time.Time,
) (currency.Amount, currency.Amount, error) {
	day, err := l.Ledger.SpentIn(ctx, agentID, ledger.DayPeriod(at), asset)
	if err != nil {
		return currency.Amount{}, currency.Amount{}, err
	}
	month, err := l.Ledger.SpentIn(ctx, agentID, ledger.MonthPeriod(at), asset)
	if err != nil {
		return currency.Amount{}, currency.Amount{}, err
	}
	return day, month, nil
}

// Debit implements payment.Ledger: customer funds to payee payable, which
// Task 7.7 settles to the on-chain float once the transfer lands. Payee
// payable is a platform bucket, since the payee is usually not a customer.
func (l PaymentLedger) Debit(ctx context.Context, d payment.Debit) (bool, error) {
	asset := d.Amount.Asset()
	funds, err := l.Ledger.EnsureAccount(ctx, &d.AccountID, ledger.TypeCustomerFunds, asset)
	if err != nil {
		return false, err
	}
	payable, err := l.Ledger.EnsureAccount(ctx, nil, ledger.TypePayeePayable, asset)
	if err != nil {
		return false, err
	}

	paymentID, agentID := d.PaymentID, d.AgentID
	_, err = l.Ledger.Post(ctx, ledger.Entry{
		Kind:          ledger.KindPayment,
		Asset:         asset,
		Lines:         ledger.Transfer(funds, payable, d.Amount),
		ReferenceType: "payment",
		ReferenceID:   &paymentID,
		AgentID:       &agentID,
		OccurredAt:    d.At,
	})
	if errors.Is(err, ledger.ErrInsufficientFunds) {
		return false, nil
	}
	return err == nil, err
}

// RecordSpend implements payment.Ledger.
func (l PaymentLedger) RecordSpend(
	ctx context.Context, agentID id.AgentID, amount currency.Amount, at time.Time,
) (currency.Amount, currency.Amount, error) {
	totals, err := l.Ledger.RecordSpend(ctx, agentID, amount, at)
	if err != nil {
		return currency.Amount{}, currency.Amount{}, err
	}
	var day, month currency.Amount
	for _, t := range totals {
		switch t.Period.Type {
		case ledger.PeriodDay:
			day = t.Amount
		case ledger.PeriodMonth:
			month = t.Amount
		}
	}
	return day, month, nil
}

// PaymentDecisions satisfies payment.DecisionLog with the decisionlog module.
type PaymentDecisions struct {
	Decisions *decisionlog.Service
}

var _ payment.DecisionLog = PaymentDecisions{}

// Record implements payment.DecisionLog.
func (p PaymentDecisions) Record(ctx context.Context, r payment.DecisionRecord) error {
	checks := make([]decisionlog.Check, len(r.Decision.Checks))
	for i, c := range r.Decision.Checks {
		checks[i] = decisionlog.Check{Stage: string(c.Stage), Result: string(c.Result)}
	}
	var ref *decisionlog.MandateRef
	if m := r.Mandate; m != nil {
		ref = &decisionlog.MandateRef{ID: m.ID, Version: m.Version, Digest: m.Digest}
	}
	_, err := p.Decisions.Record(ctx, decisionlog.Record{
		PaymentID:   r.PaymentID,
		AccountID:   r.AccountID,
		AgentID:     r.AgentID,
		Outcome:     string(r.Decision.Outcome),
		ReasonCode:  string(r.Decision.ReasonCode),
		Reason:      r.Decision.Reason,
		Mandate:     ref,
		Features:    r.Features,
		Checks:      checks,
		EvaluatedAt: r.EvaluatedAt,
		Latency:     r.Latency,
	})
	return err
}

// DecisionAccounts satisfies decisionlog.Accounts with the account module.
type DecisionAccounts struct {
	Accounts *account.Service
}

var _ decisionlog.Accounts = DecisionAccounts{}

// RequireAgentReader implements decisionlog.Accounts. Membership is the only
// thing it checks, so the session needs only who the person is.
func (a DecisionAccounts) RequireAgentReader(ctx context.Context, actor decisionlog.Actor, agentID id.AgentID) error {
	return a.Accounts.RequireAgentReader(ctx, account.Session{UserID: actor.UserID}, agentID)
}

// DecisionPayments satisfies decisionlog.Payments with the payment module.
type DecisionPayments struct {
	Payments *payment.Lookup
}

var _ decisionlog.Payments = DecisionPayments{}

// AgentPayments implements decisionlog.Payments.
func (p DecisionPayments) AgentPayments(
	ctx context.Context, agentID id.AgentID, ids []id.PaymentID,
) (map[id.PaymentID]decisionlog.Payment, error) {
	recorded, err := p.Payments.AgentPayments(ctx, agentID, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[id.PaymentID]decisionlog.Payment, len(recorded))
	for _, r := range recorded {
		out[r.ID] = decisionlog.Payment{
			Amount: r.Amount, Category: r.Category, PayTo: r.PayTo, ResourceURL: r.ResourceURL,
			Status: string(r.Status),
		}
	}
	return out, nil
}

// SummaryAccounts satisfies summary.Accounts with the account module.
type SummaryAccounts struct {
	Accounts *account.Service
}

var _ summary.Accounts = SummaryAccounts{}

// Membership implements summary.Accounts.
func (a SummaryAccounts) Membership(ctx context.Context, actor summary.Actor, accountID id.AccountID) (summary.Account, error) {
	m, err := a.Accounts.Membership(ctx, account.Session{UserID: actor.UserID}, accountID)
	if err != nil {
		return summary.Account{}, err
	}
	return summary.Account{ID: m.AccountID, Name: m.Name, Role: string(m.Role), FrozenAt: m.FrozenAt}, nil
}

// Agents implements summary.Accounts.
func (a SummaryAccounts) Agents(ctx context.Context, actor summary.Actor, accountID id.AccountID) ([]summary.Agent, error) {
	agents, err := a.Accounts.Agents(ctx, account.Session{UserID: actor.UserID}, accountID)
	if err != nil {
		return nil, err
	}
	out := make([]summary.Agent, len(agents))
	for i, g := range agents {
		out[i] = summary.Agent{ID: g.ID, Name: g.Name, Status: string(g.Status)}
	}
	return out, nil
}

// SummaryLedger satisfies summary.Ledger with the ledger module.
type SummaryLedger struct {
	Ledger *ledger.Service
}

var _ summary.Ledger = SummaryLedger{}

// Balance implements summary.Ledger, in the unit of account.
func (l SummaryLedger) Balance(ctx context.Context, accountID id.AccountID) (currency.Amount, error) {
	return l.Ledger.AccountBalance(ctx, accountID, currency.USD)
}

// Spent implements summary.Ledger.
func (l SummaryLedger) Spent(ctx context.Context, agentID id.AgentID, at time.Time) (currency.Amount, currency.Amount, error) {
	return PaymentLedger(l).Spent(ctx, agentID, currency.USD, at)
}

// Windows implements summary.Ledger.
func (l SummaryLedger) Windows(at time.Time) (time.Time, time.Time) {
	return ledger.DayPeriod(at).Start, ledger.MonthPeriod(at).Start
}

// SummaryMandates satisfies summary.Mandates with the mandate module.
type SummaryMandates struct {
	Mandates *mandate.Service
}

var _ summary.Mandates = SummaryMandates{}

// Current implements summary.Mandates. Having none is a line on the page, not
// a failure of it.
func (m SummaryMandates) Current(ctx context.Context, agentID id.AgentID) (*summary.Caps, error) {
	current, err := m.Mandates.Current(ctx, agentID)
	if errors.Is(err, mandate.ErrNoMandate) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := current.Rules
	return &summary.Caps{
		ID: current.ID, Version: current.Version,
		MaxPerTx: r.MaxPerTx, MaxPerDay: r.MaxPerDay, MaxPerMonth: r.MaxPerMonth, ExpiresAt: r.ExpiresAt,
	}, nil
}

// SummaryPayments satisfies summary.Payments with the payment module.
type SummaryPayments struct {
	Payments *payment.Lookup
}

var _ summary.Payments = SummaryPayments{}

// DeclinesSince implements summary.Payments, folding the per-asset rows into
// one entry per agent.
func (p SummaryPayments) DeclinesSince(ctx context.Context, accountID id.AccountID, since time.Time) (map[id.AgentID]summary.Declines, error) {
	totals, err := p.Payments.DeclinesSince(ctx, accountID, since)
	if err != nil {
		return nil, err
	}
	out := map[id.AgentID]summary.Declines{}
	for _, t := range totals {
		d := out[t.AgentID]
		d.Count += t.Count
		if !t.Amount.IsZero() {
			d.Amounts = append(d.Amounts, t.Amount)
		}
		out[t.AgentID] = d
	}
	return out, nil
}

// SettlementAccounts satisfies settlement.Accounts with the account module.
type SettlementAccounts struct {
	Accounts *account.Service
}

var _ settlement.Accounts = SettlementAccounts{}

// RequireMember implements settlement.Accounts. Membership is the only thing
// it checks, so the session needs only who the person is.
func (a SettlementAccounts) RequireMember(ctx context.Context, userID id.UserID, accountID id.AccountID) error {
	_, err := a.Accounts.Membership(ctx, account.Session{UserID: userID}, accountID)
	return err
}

// LedgerFundingHistory satisfies account.FundingHistory with the ledger.
type LedgerFundingHistory struct {
	Principals *account.Principals
	Ledger     *ledger.Service
}

var _ account.FundingHistory = LedgerFundingHistory{}

// FundedToDate implements account.FundingHistory, across every account the
// principal holds, so opening a second account does not reset the cap.
func (h LedgerFundingHistory) FundedToDate(ctx context.Context, principalID id.PrincipalID) (currency.Amount, error) {
	accounts, err := h.Principals.Accounts(ctx, principalID)
	if err != nil {
		return currency.Amount{}, err
	}
	return h.Ledger.FundedTotal(ctx, accounts, currency.USD)
}

// SettlementFundingGate satisfies settlement.FundingGate with the account
// module.
type SettlementFundingGate struct {
	Principals *account.Principals
	Accounts   *account.Service
}

var _ settlement.FundingGate = SettlementFundingGate{}

// FundingAllowed implements settlement.FundingGate.
//
// TODO(mvp): name the deposit's source address and chain here. Without them the
// gate applies the tier cap and skips address screening, the owner's decision
// for the devnet MVP (7 November 2026). This must change before mainnet: a
// direct deposit with no KYC is safe only behind screening (Task 2.7).
func (g SettlementFundingGate) FundingAllowed(ctx context.Context, accountID id.AccountID, amount currency.Amount) (bool, error) {
	principalID, err := g.Principals.Of(ctx, accountID)
	if err != nil {
		return false, err
	}
	err = g.Accounts.RequireFundingAllowed(ctx, account.FundingRequest{PrincipalID: principalID, Amount: amount})
	if errors.Is(err, account.ErrFundingCapExceeded) {
		return false, nil
	}
	return err == nil, err
}

// SettlementFundingLedger satisfies settlement.FundingLedger with the ledger.
type SettlementFundingLedger struct {
	Ledger *ledger.Service
}

var _ settlement.FundingLedger = SettlementFundingLedger{}

// CreditFunding implements settlement.FundingLedger: the on-chain float, which
// is negative by design, to the account's funds.
func (l SettlementFundingLedger) CreditFunding(
	ctx context.Context, accountID id.AccountID, amount currency.Amount, at time.Time,
) (id.JournalEntryID, error) {
	asset := amount.Asset()
	float, err := l.Ledger.EnsureAccount(ctx, nil, ledger.TypeOnchainFloat, asset)
	if err != nil {
		return "", err
	}
	funds, err := l.Ledger.EnsureAccount(ctx, &accountID, ledger.TypeCustomerFunds, asset)
	if err != nil {
		return "", err
	}
	return l.Ledger.Post(ctx, ledger.Entry{
		Kind: ledger.KindFunding, Asset: asset, Lines: ledger.Transfer(float, funds, amount), OccurredAt: at,
	})
}

// SettlementChain satisfies settlement.ChainReader with a Solana node, for one
// token.
type SettlementChain struct {
	Solana *solana.Client
	Mint   string
}

var _ settlement.ChainReader = SettlementChain{}

// TokenAccount implements settlement.ChainReader.
func (c SettlementChain) TokenAccount(owner string) (string, error) {
	return solana.TokenAccount(owner, c.Mint)
}

// Signatures implements settlement.ChainReader.
func (c SettlementChain) Signatures(ctx context.Context, address string, p settlement.ChainPage) ([]settlement.ChainSignature, error) {
	found, err := c.Solana.Signatures(ctx, address, solana.Page{Before: p.Before, Until: p.Until, Limit: p.Limit})
	if err != nil {
		return nil, err
	}
	out := make([]settlement.ChainSignature, len(found))
	for i, s := range found {
		out[i] = settlement.ChainSignature{Signature: s.Signature, Failed: s.Failed}
	}
	return out, nil
}

// Transaction implements settlement.ChainReader.
func (c SettlementChain) Transaction(ctx context.Context, signature, owner string) (settlement.ChainTransaction, error) {
	tx, err := c.Solana.Transaction(ctx, signature)
	if err != nil {
		return settlement.ChainTransaction{}, err
	}
	return settlement.ChainTransaction{Failed: tx.Failed, TokenChange: tx.TokenChange(owner, c.Mint)}, nil
}

// SolanaWallets is what SettlementWallets needs from a custodian: in
// production *cdp.Client, CDP's Server Wallets (Task 7.5).
type SolanaWallets interface {
	SolanaAccount(ctx context.Context, name string) (string, error)
	SignSolanaTransaction(ctx context.Context, address string, tx []byte) ([]byte, error)
}

// SettlementWallets satisfies settlement.WalletProvider with CDP Server
// Wallets, the MVP's custody model (the owner's decision of 5 November 2026).
type SettlementWallets struct {
	CDP SolanaWallets
}

var _ settlement.WalletProvider = SettlementWallets{}

// Chain implements settlement.WalletProvider.
func (SettlementWallets) Chain() settlement.Chain { return settlement.ChainSolana }

// Name implements settlement.WalletProvider.
func (SettlementWallets) Name() string { return "cdp" }

// Create implements settlement.WalletProvider. The CDP account is named by
// the account's id, which is what makes creating it idempotent: CDP keeps
// names unique in a project, and a UUID's 36 hex digits and hyphens are
// exactly the longest name it allows.
func (w SettlementWallets) Create(ctx context.Context, accountID id.AccountID) (string, error) {
	return w.CDP.SolanaAccount(ctx, string(accountID))
}

// Sign implements settlement.WalletProvider.
func (w SettlementWallets) Sign(ctx context.Context, address string, tx []byte) ([]byte, error) {
	return w.CDP.SignSolanaTransaction(ctx, address, tx)
}
