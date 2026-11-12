package conformance

import (
	"strings"
)

// DocsBase is the origin of the ORBIT documentation site, where each check
// has a page (DocURL).
const DocsBase = "https://orbit.grewelltech.com"

// summaries are the checks' one-line descriptions: what the check asks of
// the core, short enough for a progress line. Every registered check has
// one (TestEveryCheckHasSummary).
var summaries = map[string]string{
	"NGAP-UE-CTX-RELEASE-ON-REQUEST":         "UE Context Release Request gets a Command",
	"NAS-DEREG-ACCEPT-NOT-SWITCH-OFF":        "Deregistration (not switch-off) is accepted",
	"NGAP-CTX-RELEASE-AFTER-SWITCH-OFF":      "Switch-off releases the context by both IDs",
	"GTPU-END-MARKER-ON-PATH-SWITCH":         "Path switch sends an End Marker on the old tunnel",
	"GTPU-UNKNOWN-TEID-ERRIND":               "Unknown TEID gets an Error Indication",
	"GTPU-ECHO-RESPONSE":                     "GTP-U Echo Request gets an Echo Response",
	"N3-DL-QFI-CONTAINER":                    "Downlink G-PDUs carry the PDU Session Container",
	"NGAP-PATH-SWITCH-ACK-TRANSFER":          "Path Switch Acknowledge carries the transfer",
	"NGAP-HANDOVER-COMMAND-TRANSFER":         "Handover Command carries each session's transfer",
	"NGAP-HANDOVER-CANCEL-ACK":               "Handover Cancel is acknowledged",
	"NAS-SERVICE-REQUEST-REACTIVATES":        "Service Request reactivates the user plane",
	"NAS-SMC-IMEISV-REQUEST":                 "Missing IMEISV after an IMEISV request",
	"NAS-INITIAL-MSG-CONTAINER-WINS":         "The NAS message container wins over the initial message",
	"NAS-INITIAL-MSG-NO-CONTAINER":           "Security Mode Complete without the NAS container",
	"NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED":  "Initial message with non-cleartext IEs",
	"NAS-REG-REJECTED-NSSAI":                 "Unserved S-NSSAI is listed as rejected",
	"NAS-PDU-UNKNOWN-DNN":                    "Unknown DNN gets a session establishment reject",
	"NAS-PDU-RELEASE-UE-REQUESTED":           "UE-requested PDU session release",
	"NAS-PDU-SESSION-ESTABLISHMENT":          "PDU session establishment",
	"NAS-INITIAL-REGISTRATION":               "Initial registration",
	"NAS-REGISTRATION-AFTER-RELEASE":         "Registration right after the UE's context release",
	"NAS-REGISTRATION-DURING-OTHER-RELEASE":  "Registration during another UE's context release",
	"NGAP-UNKNOWN-UE-SURVIVES":               "Unknown UE NGAP IDs get an Error Indication",
	"NGAP-NG-SETUP":                          "NG Setup",
	"NGAP-DUPLICATE-RAN-NODE-ID":             "NG Setup with a duplicate RAN node ID",
	"NGAP-NGSETUP-MISSING-TALIST":            "NG Setup without a Supported TA List is refused",
	"NGAP-NGSETUP-UNKNOWN-PLMN":              "NG Setup for an unserved PLMN is refused",
	"NGAP-PAGING-ON-DL-DATA":                 "Downlink data for an idle UE pages it",
	"NGAP-RAN-CONFIG-UPDATE-ACK":             "RAN Configuration Update is acknowledged",
	"NGAP-RAN-CONFIG-UPDATE-KEEPS-TA":        "RAN Configuration Update keeps the tracking area",
	"NGAP-RAN-CONFIG-UPDATE-TRANSFER-SYNTAX": "Transfer syntax error gets an Error Indication",
	"NGAP-NG-RESET-ACK":                      "NG Reset is acknowledged",
	"NAS-SMC-NO-NIA0-REPLAYED-CAPS":          "Security Mode Command: no NIA0, capabilities replayed",
	"NAS-INTEGRITY-FAIL-DISCARD":             "Message failing integrity is discarded",
	"NAS-REPLAY-DISCARD":                     "Replayed NAS message is discarded",
	"NAS-BADMAC-REGISTRATION-AFTER-SMC":      "Bad-MAC registration after security mode is dropped",
	"NAS-RETRANSMIT-NEW-COUNT":               "Retransmissions use a new NAS COUNT",
	"NAS-T3550-ACCEPT-RETRANSMIT":            "Registration Accept is retransmitted on T3550",
	"NAS-T3560-AUTH-RETRANSMIT":              "Authentication Request is retransmitted on T3560",
	"NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL":     "MAC failure does not advance the uplink NAS COUNT",
	"NGAP-PATH-SWITCH-SECCAP-MISMATCH":       "Path switch returns the stored security capabilities",
}

// SummaryOf is the check's one-line description, or "" for an ID with none.
func SummaryOf(id string) string { return summaries[id] }

// DocURL is the address of the check's page on the documentation site.
func DocURL(id string) string {
	return DocsBase + "/conformance/checks/" + strings.ToLower(id) + "/"
}

// OutcomeURL is the address of a declared outcome's section on the check's
// page (DocURL), or the page itself when outcome is "".
func OutcomeURL(id, outcome string) string {
	if outcome == "" {
		return DocURL(id)
	}
	return DocURL(id) + "#" + outcome
}

// Category is the first segment of a group path ("NGAP / Interface
// Management Procedures / NG Setup" is "NGAP"): the protocol or
// specification a check belongs to.
func Category(group string) string {
	c, _, _ := strings.Cut(group, " / ")
	return strings.TrimSpace(c)
}
