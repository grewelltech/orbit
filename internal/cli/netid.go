package cli

import (
	"fmt"
	"os"

	"github.com/spf13/pflag"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/gnb"
)

// Help text for the network-identity flags, defined once so every command
// that dials a core describes them the same way.
const (
	mccFlagHelp = "mobile country code, 3 digits (default $ORBIT_MCC)"
	mncFlagHelp = "mobile network code, 2 or 3 digits with leading zeros kept (default $ORBIT_MNC)"
	tacFlagHelp = "tracking area code, 1-16777215"
	sstFlagHelp = "slice/service type, 0-255"
	sdFlagHelp  = "slice differentiator, 6 hex digits; empty = no SD (SD-Core uses 010203)"
	dnnFlagHelp = "data network name for the PDU session"
)

// Defaults shared by every command for the fields that have a sane one. The
// PLMN has none: a wrong-PLMN NG Setup Failure reads like a core finding, so
// the tool never guesses it.
const (
	defaultTAC = uint32(1)
	defaultSST = uint32(1)
	defaultSD  = ""
	defaultDNN = "internet"
)

// netIdentity is the network a command presents to the core: PLMN, tracking
// area, slice and (where a PDU session is involved) DNN. One struct, one set
// of flags, one validation, so the direct commands, the run commands and the
// conformance suite cannot disagree about the basics.
type netIdentity struct {
	MCC, MNC string
	TAC, SST uint32
	SD, DNN  string

	fs      *pflag.FlagSet // the flags registered by addNetIdentityFlags
	hasDNN  bool
	inherit bool // handover: unset fields are filled from the serving gNB
}

// netIDOption customises addNetIdentityFlags.
type netIDOption func(*netIdentity)

// withDNN also registers --dnn (commands that can open a PDU session).
func withDNN() netIDOption { return func(id *netIdentity) { id.hasDNN = true } }

// withInherit registers the flags with no defaults at all and no environment
// fallback: an unset field is sent empty and the server fills it from the
// UE's serving gNB (handover). The env is skipped on purpose: a handover
// target is in the serving PLMN by definition, so the UE's session is the
// source of truth, not the shell; $ORBIT_MCC/$ORBIT_MNC are a convenience
// for new attaches. Explicit flags still override, for the deliberate
// wrong-PLMN-target negative case.
func withInherit() netIDOption { return func(id *netIdentity) { id.inherit = true } }

// addNetIdentityFlags registers --mcc --mnc --tac --sst --sd (and --dnn with
// withDNN) on f. None is marked required: resolve enforces the PLMN with the
// $ORBIT_MCC/$ORBIT_MNC fallback, which cobra's required check would defeat.
func addNetIdentityFlags(f *pflag.FlagSet, id *netIdentity, opts ...netIDOption) {
	for _, o := range opts {
		o(id)
	}
	id.fs = f
	tac, sst, sd := defaultTAC, defaultSST, defaultSD
	if id.inherit {
		tac, sst = 0, 0
	}
	f.StringVar(&id.MCC, "mcc", "", mccFlagHelp)
	f.StringVar(&id.MNC, "mnc", "", mncFlagHelp)
	f.Uint32Var(&id.TAC, "tac", tac, tacFlagHelp)
	f.Uint32Var(&id.SST, "sst", sst, sstFlagHelp)
	f.StringVar(&id.SD, "sd", sd, sdFlagHelp)
	if id.hasDNN {
		f.StringVar(&id.DNN, "dnn", defaultDNN, dnnFlagHelp)
	}
}

// resolve applies the $ORBIT_MCC/$ORBIT_MNC fallback (an explicit flag wins)
// and validates every field with the shared gnb rules. With required, a
// missing PLMN is one error naming both sources. Without it (handover), the
// environment is not consulted and an entirely absent PLMN means "inherit
// from the serving gNB"; giving only one of the pair is still an error.
func (id *netIdentity) resolve(required bool) error {
	if !id.inherit {
		if id.MCC == "" {
			id.MCC = os.Getenv("ORBIT_MCC")
		}
		if id.MNC == "" {
			id.MNC = os.Getenv("ORBIT_MNC")
		}
	}
	switch {
	case id.MCC == "" && id.MNC == "":
		if required {
			return fmt.Errorf("network PLMN is required: pass --mcc/--mnc or set $ORBIT_MCC/$ORBIT_MNC")
		}
	case id.MCC == "" || id.MNC == "":
		return fmt.Errorf("--mcc and --mnc go together: pass both (or set $ORBIT_MCC/$ORBIT_MNC), or neither to inherit")
	default:
		if err := gnb.ValidatePLMN(id.MCC, id.MNC); err != nil {
			return err
		}
	}
	if !id.inherit || id.TAC != 0 {
		if err := gnb.ValidateTAC(id.TAC); err != nil {
			return err
		}
	}
	if err := gnb.ValidateSNSSAI(id.SST, id.SD); err != nil {
		return err
	}
	if id.hasDNN && id.DNN == "" {
		return fmt.Errorf("--dnn must not be empty")
	}
	return nil
}

// sliceGiven reports whether the operator set --sst or --sd. In inherit mode
// an untouched slice is sent empty so the server copies the serving gNB's.
func (id *netIdentity) sliceGiven() bool {
	if id.fs == nil {
		return true
	}
	return id.fs.Changed("sst") || id.fs.Changed("sd")
}

// gnbConfig builds the engine-side gNB config for this identity.
func (id netIdentity) gnbConfig(gnbID uint32, idBits int, name string) gnb.Config {
	return gnb.Config{
		ID: gnbID, IDBits: idBits, Name: name,
		MCC: id.MCC, MNC: id.MNC, TAC: id.TAC,
		Slices: []gnb.SNSSAI{{SST: uint8(id.SST), SD: id.SD}},
	}
}

// gnbProto builds the API gNB config for this identity. In inherit mode a
// slice the operator did not set is omitted (empty = inherit).
func (id netIdentity) gnbProto(gnbID, idBits uint32, name string) *orbitv1.GnbConfig {
	p := &orbitv1.GnbConfig{Id: gnbID, IdBits: idBits, Name: name, Mcc: id.MCC, Mnc: id.MNC, Tac: id.TAC}
	if !id.inherit || id.sliceGiven() {
		p.Slices = []*orbitv1.Snssai{{Sst: id.SST, Sd: id.SD}}
	}
	return p
}

// String renders the identity for the one-line `network:` report every
// command prints, e.g. "plmn=001/01 tac=1 snssai=1/010203 dnn=internet".
// Fields left for the server to inherit are shown as such.
func (id netIdentity) String() string {
	plmn := id.MCC + "/" + id.MNC
	if id.MCC == "" && id.MNC == "" {
		plmn = "(inherited)"
	}
	tac := fmt.Sprint(id.TAC)
	if id.inherit && id.TAC == 0 {
		tac = "(inherited)"
	}
	snssai := fmt.Sprint(id.SST)
	if id.SD != "" {
		snssai += "/" + id.SD
	}
	if id.inherit && !id.sliceGiven() {
		snssai = "(inherited)"
	}
	s := fmt.Sprintf("plmn=%s tac=%s snssai=%s", plmn, tac, snssai)
	if id.hasDNN {
		s += " dnn=" + id.DNN
	}
	return s
}
