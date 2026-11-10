// Package testnet allocates the loopback blocks tests bind fixed addresses
// in, and says how a test treats a bind that fails.
//
// A test that needs fixed N3 addresses (a stub UPF on .10:2152, gNB N3 on .11
// and .12) takes a 127.0.N.0/24 of its own from the table below. `go test
// ./...` runs packages at once in one network namespace, so two tests in
// different packages on one block bind the same address at the same time.
// TestBlocksAreUnique fails on a block given twice, and TestNoLiteralBlocks
// on a 127.0.N. address written anywhere but here.
package testnet

import "fmt"

// Blocks, one per test (or table row) that binds fixed addresses in it.
const (
	// internal/engine
	EngineUserPlane           = 41 // TestMockPairPDUSessionCarriesUserPlane
	EngineXnEndMarker         = 42 // TestMockPairXnPathSwitch/end_marker
	EngineXnNoEndMarker       = 43 // TestMockPairXnPathSwitch/no_end_marker
	EngineXnLateGPDU          = 44 // TestMockPairXnPathSwitch/late_g-pdu_after_the_end_marker
	EngineXnEndMarkerQFI      = 45 // TestMockPairXnPathSwitch/end_marker_with_QFI_header
	EngineXnULReallocated     = 46 // TestMockPairXnPathSwitch/uplink_tunnel_reallocated
	EngineObserveAcrossXn     = 47 // TestObserveDownlinkAcrossXn
	EngineLatency             = 48 // TestLatencyStopsWhenDeregistered
	EngineNASAlgorithmPairs   = 49 // TestAttachEveryNASAlgorithmPair
	EngineN3PoolDistinct      = 50 // TestN3PoolRefcountLifecycle
	EngineTwoUEsOneGNB        = 51 // TestTwoUEsOneGNBNoEADDRINUSE
	EngineRebindInterGNB      = 52 // TestRebindDataPathInterGNB
	EngineRebindLastUEGrace   = 53 // TestRebindDataPathInterGNBLastUEEndMarkerGrace
	EngineRetargetConcurrent  = 54 // TestRetargetDataPathConcurrentDataplane
	EngineManagerFleetShare   = 55 // TestManagerAndFleetShareOneGNBSocket
	EngineHandoverInherit     = 71 // TestHandoverResultReportsPresentedTarget/inherit
	EngineHandoverExplicitTAC = 72 // TestHandoverResultReportsPresentedTarget/explicit_TAC
	EngineUplinkRetryStale    = 73 // TestSendUplinkRetriesOnceOnStaleView
	EngineUplinkTeardown      = 74 // TestSendUplinkTeardownDoesNotRetry
	EngineUplinkRetryNoLoop   = 75 // TestSendUplinkRetryDoesNotLoop
	EngineUplinkGone          = 76 // TestSendUplinkMarksSessionGone
	EngineNetstackInterGNB    = 77 // TestInterGNBMoveResetsNetstackConns
	EngineRegisterCleanup     = 78 // TestRegisterCleanupReportsAHeldN3Socket

	// internal/conformance
	ConformanceNonCanonicalV4   = 56 // TestEndMarkerCheckNonCanonicalGNBN3/IPv4-mapped
	ConformanceNonCanonicalHex  = 57 // TestEndMarkerCheckNonCanonicalGNBN3/IPv4-mapped,_expanded
	ConformanceNoEndMarker      = 61 // TestEndMarkerCheckAgainstMockPair/end_marker_disabled
	ConformanceEndMarker        = 62 // TestEndMarkerCheckAgainstMockPair/end_marker
	ConformanceEndMarkerQFI     = 63 // TestEndMarkerCheckAgainstMockPair/end_marker_with_QFI
	ConformanceLateGPDU         = 64 // TestEndMarkerCheckAgainstMockPair/late_g-pdu_after_the_end_marker
	ConformanceRegistrationFail = 65 // TestEndMarkerCheckRegistrationFailure
	ConformanceTimeoutBefore    = 66 // TestEndMarkerCheckTimeoutBeforeWindow
	ConformanceTraffic          = 67 // TestEndMarkerCheckAgainstMockPair/traffic_configured
	ConformanceCipherNEA0       = 68 // TestUELevelChecksUnderNEA2, the End Marker row under NEA0
	ConformanceCipherNEA2       = 69 // TestUELevelChecksUnderNEA2, the End Marker row under NEA2

	// internal/cli
	CLIProbePasses    = 83 // TestTargetProbePasses
	CLIProbeFailures  = 84 // TestTargetProbeFailures
	CLIProbeUserPlane = 85 // TestTargetProbeUserPlane
	CLITargetInit     = 86 // TestTargetInit
	CLIScenariosRun   = 87 // TestScenariosRunWithTarget
)

// Blocks of the mobility conformance checks' tests, in internal/engine and
// internal/conformance.
const (
	EngineXnAckKeptUL     = 200 // TestXnHandoverReportsTheAcknowledge/uplink_tunnel_kept
	EngineXnAckNewUL      = 201 // TestXnHandoverReportsTheAcknowledge/uplink_tunnel_reallocated
	EngineXnAckNoTransfer = 202 // TestXnHandoverReportsTheAcknowledge/transfer_omitted
	EngineXnNotAckFailure = 203 // TestXnHandoverNotAcknowledged/failure
	EngineXnNotAckSilence = 204 // TestXnHandoverNotAcknowledged/no_answer
	EngineN2Completes     = 205 // TestN2HandoverOverMockPair/completes
	EngineN2NoList        = 206 // TestN2HandoverOverMockPair/command_without_the_list
	EngineN2PrepFailure   = 207 // TestN2HandoverPreparationFails/preparation_failure
	EngineN2Unanswered    = 208 // TestN2HandoverPreparationFails/no_answer

	ConformancePSAckPlain      = 190 // TestPathSwitchAckCheckAgainstMockPair/acknowledged,_no_traffic
	ConformancePSAckEcho       = 191 // TestPathSwitchAckCheckAgainstMockPair/acknowledged,_uplink_answered
	ConformancePSAckNewUL      = 192 // TestPathSwitchAckCheckAgainstMockPair/new_uplink_endpoint,_uplink_answered
	ConformancePSAckSilentPeer = 193 // TestPathSwitchAckCheckAgainstMockPair/echo_peer_silent
	ConformancePSAckNoTransfer = 194 // TestPathSwitchAckCheckAgainstMockPair/transfer_omitted
	ConformancePSAckHiddenUL   = 195 // TestPathSwitchAckCheckAgainstMockPair/uplink_endpoint_moved_without_UL_TNL
	ConformancePSAckFailure    = 196 // TestPathSwitchAckCheckAgainstMockPair/path_switch_request_failure
	ConformancePSAckIgnored    = 197 // TestPathSwitchAckCheckAgainstMockPair/path_switch_request_unanswered
	ConformancePSAckRegFail    = 198 // TestPathSwitchAckCheckErrors/registration_refused
	ConformancePSAckTimeout    = 199 // TestPathSwitchAckCheckErrors/timeout_in_the_uplink_window

	ConformanceHOCmdPass       = 210 // TestHandoverCommandCheckAgainstMockPair/command_with_the_list
	ConformanceHOCmdNoList     = 211 // TestHandoverCommandCheckAgainstMockPair/list_dropped
	ConformanceHOCmdNoTransfer = 212 // TestHandoverCommandCheckAgainstMockPair/transfer_omitted
	ConformanceHOCmdBadTransfe = 213 // TestHandoverCommandCheckAgainstMockPair/transfer_undecodable
	ConformanceHOCmdPrepFail   = 214 // TestHandoverCommandCheckAgainstMockPair/preparation_failure
	ConformanceHOCmdUnanswered = 215 // TestHandoverCommandCheckAgainstMockPair/handover_required_unanswered
	ConformanceHOCmdRegFail    = 216 // TestHandoverCommandCheckRegistrationFailure
)

// Base is block n's address prefix, "127.0.n." (append the host part).
func Base(n int) string { return fmt.Sprintf("127.0.%d.", n) }

// Addr is host h in block n, "127.0.n.h".
func Addr(n, h int) string { return fmt.Sprintf("127.0.%d.%d", n, h) }
