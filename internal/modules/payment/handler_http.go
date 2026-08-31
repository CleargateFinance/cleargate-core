package payment

// The only file in this module that knows Gin exists.
// Handlers decode, delegate to Service, and encode. No business logic here.
// TODO(scaffold).

type handler struct{ svc *Service }
