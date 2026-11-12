package mockamf

// WithRegistrationReject makes the mock answer every Security Mode Complete
// with a protected Registration Reject with 5GMM cause cause, in place of
// the Registration Accept: an AMF that cannot accept the subscriber's
// registration (TS 24.501 5.5.1.2.5). Zero leaves the Accept.
func WithRegistrationReject(cause uint8) Option {
	return func(a *AMF) { a.registrationReject = cause }
}
