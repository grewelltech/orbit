package conformance

// Archive-verified versions shared by references into the same
// specification. Each list holds the version a requirement first appears in
// and the latest version of every release through LatestRelease, read in
// https://www.3gpp.org/ftp/Specs/archive/. The quoted normative sentences and
// the full change history are in docs/conformance/provenance/<CHECK-ID>.md.

// ts38413Verified covers TS 38.413 (NGAP): V15.0.0, where every cited NGAP
// requirement first appears, and the latest version of Rel-15 to Rel-19.
var ts38413Verified = []SpecVersion{
	{Release: 15, Version: "V15.0.0", Archive: "38413-f00.zip"},
	{Release: 15, Version: "V15.13.0", Archive: "38413-fd0.zip"},
	{Release: 16, Version: "V16.18.0", Archive: "38413-gi0.zip"},
	{Release: 17, Version: "V17.15.0", Archive: "38413-hf0.zip"},
	{Release: 18, Version: "V18.11.0", Archive: "38413-ib0.zip"},
	{Release: 19, Version: "V19.4.0", Archive: "38413-j40.zip"},
}

// ts29281Verified covers TS 29.281 (GTP-U): V8.0.0, where the Error
// Indication requirement first appears; V15.4.0, which adds "PDU Session";
// and the latest version of Rel-15 to Rel-19.
var ts29281Verified = []SpecVersion{
	{Release: 8, Version: "V8.0.0", Archive: "29281-800.zip"},
	{Release: 15, Version: "V15.4.0", Archive: "29281-f40.zip"},
	{Release: 15, Version: "V15.7.0", Archive: "29281-f70.zip"},
	{Release: 16, Version: "V16.2.0", Archive: "29281-g20.zip"},
	{Release: 17, Version: "V17.5.0", Archive: "29281-h50.zip"},
	{Release: 18, Version: "V18.4.0", Archive: "29281-i40.zip"},
	{Release: 19, Version: "V19.2.0", Archive: "29281-j20.zip"},
}

// ts23501Verified covers TS 23.501 (5GS architecture): V15.0.0, where the
// End Marker construction requirement (5.8.2.9.1) first appears, and the
// latest version of Rel-15 to Rel-19.
var ts23501Verified = []SpecVersion{
	{Release: 15, Version: "V15.0.0", Archive: "23501-f00.zip"},
	{Release: 15, Version: "V15.13.0", Archive: "23501-fd0.zip"},
	{Release: 16, Version: "V16.20.0", Archive: "23501-gk0.zip"},
	{Release: 17, Version: "V17.15.0", Archive: "23501-hf0.zip"},
	{Release: 18, Version: "V18.12.0", Archive: "23501-ic0.zip"},
	{Release: 19, Version: "V19.9.0", Archive: "23501-j90.zip"},
}

// ts29281EndMarkerVerified covers TS 29.281 7.3.2 (End Marker): V8.0.0,
// where message type 254 first appears; V15.2.0, which adds the 5GS
// sentence; and the latest version of Rel-15 to Rel-19.
var ts29281EndMarkerVerified = []SpecVersion{
	{Release: 8, Version: "V8.0.0", Archive: "29281-800.zip"},
	{Release: 15, Version: "V15.2.0", Archive: "29281-f20.zip"},
	{Release: 15, Version: "V15.7.0", Archive: "29281-f70.zip"},
	{Release: 16, Version: "V16.2.0", Archive: "29281-g20.zip"},
	{Release: 17, Version: "V17.5.0", Archive: "29281-h50.zip"},
	{Release: 18, Version: "V18.4.0", Archive: "29281-i40.zip"},
	{Release: 19, Version: "V19.2.0", Archive: "29281-j20.zip"},
}

// ts29244Verified covers TS 29.244 (PFCP): V14.0.0, where SNDEM (8.2.31)
// first appears, and the latest version of Rel-15 to Rel-19.
var ts29244Verified = []SpecVersion{
	{Release: 14, Version: "V14.0.0", Archive: "29244-e00.zip"},
	{Release: 15, Version: "V15.10.0", Archive: "29244-fa0.zip"},
	{Release: 16, Version: "V16.13.0", Archive: "29244-gd0.zip"},
	{Release: 17, Version: "V17.10.0", Archive: "29244-ha0.zip"},
	{Release: 18, Version: "V18.10.0", Archive: "29244-ia0.zip"},
	{Release: 19, Version: "V19.6.0", Archive: "29244-j60.zip"},
}
