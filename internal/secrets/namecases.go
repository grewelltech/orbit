package secrets

// NameCases are the names IsSecretName is held to, and whether each names
// a secret: this package's test checks them, and the dashboard's port of
// IsSecretName is checked against the same table (generated into
// web/src/data/launch/runschema.gen.ts), so a rule change here fails there
// until the port follows.
var NameCases = map[string]bool{
	"ki": true, "opc": true, "k": true, "token": true, "Password": true, "secret": true,
	"apiKey": true, "api-key": true, "X-Api-Key": true, "x_api_key": true, "API_KEY": true,
	"bearer": true, "my-bearer": true, "Authorization": true, "auth_token": true, "authToken": true,
	"passwd": true, "passphrase": true, "privateKey": true, "access_key": true, "credential": true,
	"loom_token": true, "routing_indicator": false, "supi": false, "monkey": false, "keyboard": false,
	"tokens_per_s": false, "object_size": false, "oauth": false, "name": false, "": false,
	// Dots and spaces split words, as '-' and '_' do; a Unicode digit or
	// lower-case letter before an upper-case one splits too.
	"api.key": true, "x.token": true, "my token": true, "bearer token": true, "Private.Key": true,
	"my.Bearer": true, "token ": true, "x٣Token": true, "a.b": false, "tokenizer": false,
	"my token count": false, "Tokenizer.Mode": false,
	// Runes Go lowers to ASCII though JS's toLowerCase does not do the
	// same: U+0130 İ (to "i") and U+212A, the Kelvin sign (to "k").
	"k\u0130": true, "K\u0130": true, "k-\u0130": true, "K-\u0130": true, "AP\u0130KEY": true, "x\u212Ai": true,
}
