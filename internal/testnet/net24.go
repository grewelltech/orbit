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
	EngineUserPlane           = 41  // TestMockPairPDUSessionCarriesUserPlane
	EngineXnEndMarker         = 42  // TestMockPairXnPathSwitch/end_marker
	EngineXnNoEndMarker       = 43  // TestMockPairXnPathSwitch/no_end_marker
	EngineXnLateGPDU          = 44  // TestMockPairXnPathSwitch/late_g-pdu_after_the_end_marker
	EngineXnEndMarkerQFI      = 45  // TestMockPairXnPathSwitch/end_marker_with_QFI_header
	EngineXnULReallocated     = 46  // TestMockPairXnPathSwitch/uplink_tunnel_reallocated
	EngineObserveAcrossXn     = 47  // TestObserveDownlinkAcrossXn
	EngineLatency             = 48  // TestLatencyStopsWhenDeregistered
	EngineNASAlgorithmPairs   = 49  // TestAttachEveryNASAlgorithmPair
	EngineN3PoolDistinct      = 50  // TestN3PoolRefcountLifecycle
	EngineTwoUEsOneGNB        = 51  // TestTwoUEsOneGNBNoEADDRINUSE
	EngineRebindInterGNB      = 52  // TestRebindDataPathInterGNB
	EngineRebindLastUEGrace   = 53  // TestRebindDataPathInterGNBLastUEEndMarkerGrace
	EngineRetargetConcurrent  = 54  // TestRetargetDataPathConcurrentDataplane
	EngineManagerFleetShare   = 55  // TestManagerAndFleetShareOneGNBSocket
	EngineHandoverInherit     = 71  // TestHandoverResultReportsPresentedTarget/inherit
	EngineHandoverExplicitTAC = 72  // TestHandoverResultReportsPresentedTarget/explicit_TAC
	EngineUplinkRetryStale    = 73  // TestSendUplinkRetriesOnceOnStaleView
	EngineUplinkTeardown      = 74  // TestSendUplinkTeardownDoesNotRetry
	EngineUplinkRetryNoLoop   = 75  // TestSendUplinkRetryDoesNotLoop
	EngineUplinkGone          = 76  // TestSendUplinkMarksSessionGone
	EngineNetstackInterGNB    = 77  // TestInterGNBMoveResetsNetstackConns
	EngineRegisterCleanup     = 78  // TestRegisterCleanupReportsAHeldN3Socket
	EngineIdlePaging          = 120 // TestIdlePagingServiceRequest/NEA0
	EngineIdlePagingNEA2      = 121 // TestIdlePagingServiceRequest/NEA2
	EngineIdleReject          = 122 // TestServiceRequestAnswers/reject
	EngineIdleNoUserPlane     = 123 // TestServiceRequestAnswers/accept_without_user_plane
	EngineIdleUnanswered      = 124 // TestServiceRequestAnswers/unanswered
	EngineIdleSignalling      = 125 // TestServiceRequestAnswers/signalling_only
	EngineIdleWrongIdentity   = 126 // TestObservePaging/another_identity
	EngineIdleNoPaging        = 127 // TestObservePaging/no_paging
	EngineIdleNoGUTI          = 128 // TestIdleNeedsGUTI
	EngineIdleUPFNotTold      = 129 // TestServiceRequestAnswers/UPF_not_told

	// internal/conformance
	ConformanceNonCanonicalV4   = 56  // TestEndMarkerCheckNonCanonicalGNBN3/IPv4-mapped
	ConformanceNonCanonicalHex  = 57  // TestEndMarkerCheckNonCanonicalGNBN3/IPv4-mapped,_expanded
	ConformanceNoEndMarker      = 61  // TestEndMarkerCheckAgainstMockPair/end_marker_disabled
	ConformanceEndMarker        = 62  // TestEndMarkerCheckAgainstMockPair/end_marker
	ConformanceEndMarkerQFI     = 63  // TestEndMarkerCheckAgainstMockPair/end_marker_with_QFI
	ConformanceLateGPDU         = 64  // TestEndMarkerCheckAgainstMockPair/late_g-pdu_after_the_end_marker
	ConformanceRegistrationFail = 65  // TestEndMarkerCheckRegistrationFailure
	ConformanceTimeoutBefore    = 66  // TestEndMarkerCheckTimeoutBeforeWindow
	ConformanceTraffic          = 67  // TestEndMarkerCheckAgainstMockPair/traffic_configured
	ConformanceCipherNEA0       = 68  // TestUELevelChecksUnderNEA2, the End Marker row under NEA0
	ConformanceCipherNEA2       = 69  // TestUELevelChecksUnderNEA2, the End Marker row under NEA2
	ConformancePagingPass       = 130 // TestPagingCheckAgainstMockPair/paging
	ConformancePagingWithheld   = 131 // TestPagingCheckAgainstMockPair/paging_withheld
	ConformancePagingWrongID    = 132 // TestPagingCheckAgainstMockPair/another_identity
	ConformancePagingNoRelease  = 133 // TestPagingCheckAgainstMockPair/release_request_ignored
	ConformancePagingNoGUTI     = 134 // TestPagingCheckAgainstMockPair/no_5G-GUTI
	ConformancePagingNoTrigger  = 135 // TestPagingCheckAgainstMockPair/trigger_fails
	ConformancePagingCutShort   = 136 // TestPagingCheckCutShort
	ConformanceServicePass      = 140 // TestServiceCheckAgainstMockPair/reactivated
	ConformanceServiceReject    = 141 // TestServiceCheckAgainstMockPair/reject
	ConformanceServiceNoUP      = 142 // TestServiceCheckAgainstMockPair/accept_without_user_plane
	ConformanceServiceUPFStale  = 143 // TestServiceCheckAgainstMockPair/UPF_not_told
	ConformanceServiceIgnored   = 144 // TestServiceCheckAgainstMockPair/unanswered
	ConformanceServiceNoTraffic = 145 // TestServiceCheckAgainstMockPair/no_traffic_before_the_release
	ConformanceServiceNoTrigger = 146 // TestServiceCheckAgainstMockPair/trigger_fails
	ConformanceServiceBusy      = 147 // TestServiceCheckAgainstMockPair/traffic_never_stops
	ConformanceServiceNoRelease = 148 // TestServiceCheckAgainstMockPair/release_request_ignored
	ConformanceServiceNoGUTI    = 149 // TestServiceCheckAgainstMockPair/no_5G-GUTI
	ConformanceServiceCutShort  = 150 // TestServiceCheckCutShort

	// internal/conformance, the UE-requested PDU session release check
	ConformancePDURelease       = 151 // TestPDUReleaseCheckAgainstMockPair/released
	ConformancePDUReleaseApart  = 152 // TestPDUReleaseCheckAgainstMockPair/released,_the_commands_apart
	ConformancePDUReleaseSilent = 153 // TestPDUReleaseCheckAgainstMockPair/no_answer
	ConformancePDUReleaseNAS    = 154 // TestPDUReleaseCheckAgainstMockPair/the_NAS_command_alone
	ConformancePDUReleaseNGAP   = 155 // TestPDUReleaseCheckAgainstMockPair/the_NGAP_command_alone
	ConformancePDUReleaseReject = 156 // TestPDUReleaseCheckAgainstMockPair/rejected
	ConformancePDUReleaseLate   = 157 // TestPDUReleaseCheckAgainstMockPair/downlink_continues
	ConformancePDUReleaseGrace  = 158 // TestPDUReleaseCheckAgainstMockPair/downlink_in_flight_at_the_release
	ConformancePDUReleaseFlow   = 159 // TestPDUReleaseCheckAgainstMockPair/traffic_configured
	ConformancePDUReleaseCut    = 160 // TestPDUReleaseCheckCutShort
	ConformancePDUReleaseNEA0   = 161 // TestUELevelChecksUnderNEA2, the PDU release row under NEA0
	ConformancePDUReleaseNEA2   = 162 // TestUELevelChecksUnderNEA2, the PDU release row under NEA2

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
	EngineN2FailAfterAck  = 209 // TestN2HandoverPreparationFails/failure_after_the_acknowledge
	EngineReleaseOnReq    = 227 // TestRequestContextReleaseOverMockPair/released
	EngineReleaseIgnored  = 228 // TestRequestContextReleaseOverMockPair/request_ignored
	EngineCancelAcked     = 220 // TestCancelHandoverOverMockPair/acknowledged
	EngineCancelNoAck     = 221 // TestCancelHandoverOverMockPair/acknowledge_withheld
	EngineCancelNoPrep    = 222 // TestCancelHandoverNotPrepared
	EngineCancelLost      = 248 // TestCancelHandoverSourceLost

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
	ConformanceHOCmdFailAcked  = 217 // TestHandoverCommandCheckAgainstMockPair/preparation_failure_after_the_acknowledge

	ConformanceRelOnReq   = 230 // TestUECtxReleaseCheckAgainstMock/command
	ConformanceRelIgnored = 231 // TestUECtxReleaseCheckAgainstMock/command_withheld
	ConformanceRelNoGUTI  = 232 // TestUECtxReleaseCheckAgainstMock/command,_no_5G-GUTI
	ConformanceRelRegFail = 233 // TestUECtxReleaseCheckRegistrationFailure

	ConformanceQFIPresent = 235 // TestQFIContainerCheckAgainstMockPair/container_present
	ConformanceQFIAbsent  = 236 // TestQFIContainerCheckAgainstMockPair/container_absent
	ConformanceQFISilent  = 237 // TestQFIContainerCheckAgainstMockPair/echo_peer_silent
	ConformanceQFIRegFail = 238 // TestQFIContainerCheckErrors/registration_refused
	ConformanceQFITimeout = 239 // TestQFIContainerCheckErrors/timeout_while_sampling

	ConformanceCancelAcked   = 223 // TestHandoverCancelCheckAgainstMockPair/acknowledged
	ConformanceCancelNoAck   = 224 // TestHandoverCancelCheckAgainstMockPair/acknowledge_withheld
	ConformanceCancelNoPrep  = 225 // TestHandoverCancelCheckAgainstMockPair/preparation_failure
	ConformanceCancelRegFail = 226 // TestHandoverCancelCheckRegistrationFailure
	ConformanceCancelCrash   = 229 // TestHandoverCancelCheckAgainstMockPair/stops_answering

	ConformanceSecCapStored  = 240 // TestPathSwitchSecCapCheckAgainstMockPair/stored_capabilities
	ConformanceSecCapAbsent  = 241 // TestPathSwitchSecCapCheckAgainstMockPair/no_capabilities
	ConformanceSecCapEchoed  = 242 // TestPathSwitchSecCapCheckAgainstMockPair/echoed_capabilities
	ConformanceSecCapFailure = 243 // TestPathSwitchSecCapCheckAgainstMockPair/path_switch_request_failure
	ConformanceSecCapIgnored = 244 // TestPathSwitchSecCapCheckAgainstMockPair/path_switch_request_unanswered
	ConformanceSecCapCrash   = 245 // TestPathSwitchSecCapCheckAgainstMockPair/stops_answering
	ConformanceSecCapRegFail = 246 // TestPathSwitchSecCapCheckRegistrationFailure
	EngineXnCapsOption       = 247 // TestXnHandoverWithPathSwitchCapabilities
)

// Base is block n's address prefix, "127.0.n." (append the host part).
func Base(n int) string { return fmt.Sprintf("127.0.%d.", n) }

// Addr is host h in block n, "127.0.n.h".
func Addr(n, h int) string { return fmt.Sprintf("127.0.%d.%d", n, h) }
