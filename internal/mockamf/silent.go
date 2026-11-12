package mockamf

// WithSilentAfterNGSetup makes the mock answer NG Setup Requests and ignore
// every other PDU, undecodable ones included: a core that accepts a gNB
// and then answers nothing, so every procedure after the NG Setup waits
// out its timer.
func WithSilentAfterNGSetup() Option { return func(a *AMF) { a.silent = true } }
