package app

// BuildWorker constructs the background application: settlement, counterparty
// scoring, agent baselining, notifications, outbox relay.
//
// Same modules as the API, different wiring. Anything slow is registered only
// here, which turns "nothing slow in the request path" (§6.6) from a code
// review convention into a fact about the binary.
//
// TODO(scaffold): BuildWorker(cfg) (*Worker, func(), error).
