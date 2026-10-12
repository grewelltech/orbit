package cli

import (
	"fmt"
	"os"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// Help text for the --ki / --opc flags on every command that takes
// subscriber credentials, so each one states the env fallback the same way.
const (
	kiFlagHelp  = "subscriber key Ki (32 hex; default $ORBIT_KI)"
	opcFlagHelp = "operator key OPc (32 hex; default $ORBIT_OPC)"
)

// resolveCreds returns validated Ki/OPc, falling back to $ORBIT_KI/$ORBIT_OPC
// when a flag is absent, so every command that takes credentials (ue
// register, load, runs start-load, runs start-fleet) acquires and validates
// them the same way — and fails locally with a clear message rather than
// sending empty or malformed keys the server rejects opaquely. An explicit
// flag wins over the environment.
//
// Errors name the flags and variables, never a key value.
func resolveCreds(ki, opc string) (string, string, error) {
	if ki == "" {
		ki = os.Getenv("ORBIT_KI")
	}
	if opc == "" {
		opc = os.Getenv("ORBIT_OPC")
	}
	if ki == "" || opc == "" {
		return "", "", fmt.Errorf("subscriber keys are required: pass --ki/--opc or set $ORBIT_KI/$ORBIT_OPC")
	}
	if _, err := auth.ParseHexKey("Ki", ki); err != nil {
		return "", "", err
	}
	if _, err := auth.ParseHexKey("OPc", opc); err != nil {
		return "", "", err
	}
	return ki, opc, nil
}

// gnbN3Flag validates and canonicalises a --gnb-n3 value with the shared
// gnb.CanonicalN3 rule ("" stays "").
func gnbN3Flag(v string) (string, error) {
	n3, err := gnb.CanonicalN3(v)
	if err != nil {
		return "", fmt.Errorf("--gnb-n3: %w", err)
	}
	return n3, nil
}
