package asdu

import "strconv"

// TypeID identifies what an ASDU carries. It is the first octet of every ASDU.
type TypeID uint8

// Monitor direction — process information, station to master.
//
// Each measurand exists in three forms: without a time tag (_NA/_NB/_NC), with a
// CP24Time2a short time tag (_TA/_TB/_TC), and with a full CP56Time2a tag
// (_TD/_TE/_TF). Only the first and third are legal on 104; see forbidden.go.
const (
	MSpNa1 TypeID = 1  // single point
	MDpNa1 TypeID = 3  // double point: OFF, ON, and two indeterminate states
	MStNa1 TypeID = 5  // step position, -64..63, with a transient flag
	MBoNa1 TypeID = 7  // bitstring of 32 bits
	MMeNa1 TypeID = 9  // measured, normalised (-1..+1)
	MMeNb1 TypeID = 11 // measured, scaled (-32768..32767)
	MMeNc1 TypeID = 13 // measured, short float
	MItNa1 TypeID = 15 // integrated totals (a counter)
	MPsNa1 TypeID = 20 // packed single points with change detection
	MMeNd1 TypeID = 21 // normalised WITHOUT a quality descriptor

	MSpTb1 TypeID = 30 // single point, CP56Time2a
	MDpTb1 TypeID = 31 // double point, CP56Time2a
	MStTb1 TypeID = 32 // step position, CP56Time2a
	MBoTb1 TypeID = 33 // bitstring, CP56Time2a
	MMeTd1 TypeID = 34 // normalised, CP56Time2a
	MMeTe1 TypeID = 35 // scaled, CP56Time2a
	MMeTf1 TypeID = 36 // short float, CP56Time2a
	MItTb1 TypeID = 37 // integrated totals, CP56Time2a
	MEpTd1 TypeID = 38 // protection equipment event, CP56Time2a
	MEpTe1 TypeID = 39 // protection equipment packed start events, CP56Time2a
	MEpTf1 TypeID = 40 // protection equipment packed output circuit, CP56Time2a
)

// Control direction — commands, master to station.
const (
	CScNa1 TypeID = 45 // single command
	CDcNa1 TypeID = 46 // double command
	CRcNa1 TypeID = 47 // regulating step command
	CSeNa1 TypeID = 48 // set point, normalised
	CSeNb1 TypeID = 49 // set point, scaled
	CSeNc1 TypeID = 50 // set point, short float
	CBoNa1 TypeID = 51 // bitstring of 32 bits

	CScTa1 TypeID = 58 // single command, CP56Time2a
	CDcTa1 TypeID = 59 // double command, CP56Time2a
	CRcTa1 TypeID = 60 // regulating step command, CP56Time2a
	CSeTa1 TypeID = 61 // set point normalised, CP56Time2a
	CSeTb1 TypeID = 62 // set point scaled, CP56Time2a
	CSeTc1 TypeID = 63 // set point short float, CP56Time2a
	CBoTa1 TypeID = 64 // bitstring, CP56Time2a
)

// System information, both directions.
const (
	MEiNa1 TypeID = 70  // end of initialisation (station to master)
	CIcNa1 TypeID = 100 // interrogation command - the GI that fills the cache
	CCiNa1 TypeID = 101 // counter interrogation command
	CRdNa1 TypeID = 102 // read command
	CCsNa1 TypeID = 103 // clock synchronisation command
	CRpNa1 TypeID = 105 // reset process command
)

// typeNames gives the standard mnemonic for the types this service handles. A
// type absent from the map is printed as its number, which is what an
// unsupported-type log line needs to say anyway.
var typeNames = map[TypeID]string{
	MSpNa1: "M_SP_NA_1", MDpNa1: "M_DP_NA_1", MStNa1: "M_ST_NA_1", MBoNa1: "M_BO_NA_1",
	MMeNa1: "M_ME_NA_1", MMeNb1: "M_ME_NB_1", MMeNc1: "M_ME_NC_1", MItNa1: "M_IT_NA_1",
	MPsNa1: "M_PS_NA_1", MMeNd1: "M_ME_ND_1",
	MSpTb1: "M_SP_TB_1", MDpTb1: "M_DP_TB_1", MStTb1: "M_ST_TB_1", MBoTb1: "M_BO_TB_1",
	MMeTd1: "M_ME_TD_1", MMeTe1: "M_ME_TE_1", MMeTf1: "M_ME_TF_1", MItTb1: "M_IT_TB_1",
	MEpTd1: "M_EP_TD_1", MEpTe1: "M_EP_TE_1", MEpTf1: "M_EP_TF_1",

	CScNa1: "C_SC_NA_1", CDcNa1: "C_DC_NA_1", CRcNa1: "C_RC_NA_1", CSeNa1: "C_SE_NA_1",
	CSeNb1: "C_SE_NB_1", CSeNc1: "C_SE_NC_1", CBoNa1: "C_BO_NA_1",
	CScTa1: "C_SC_TA_1", CDcTa1: "C_DC_TA_1", CRcTa1: "C_RC_TA_1", CSeTa1: "C_SE_TA_1",
	CSeTb1: "C_SE_TB_1", CSeTc1: "C_SE_TC_1", CBoTa1: "C_BO_TA_1",

	MEiNa1: "M_EI_NA_1", CIcNa1: "C_IC_NA_1", CCiNa1: "C_CI_NA_1", CRdNa1: "C_RD_NA_1",
	CCsNa1: "C_CS_NA_1", CRpNa1: "C_RP_NA_1",
}

func (t TypeID) String() string {
	if name, ok := typeNames[t]; ok {
		return name
	}
	return "TypeID(" + strconv.Itoa(int(t)) + ")"
}

// IsMonitor reports whether the type carries process information from the
// station. The split is by number: 1..44 is the monitor direction.
func (t TypeID) IsMonitor() bool { return t >= 1 && t <= 44 }

// IsControl reports whether the type is a command towards the station.
func (t TypeID) IsControl() bool { return t >= 45 && t <= 69 }

// IsContact reports whether the type carries the state of a contact:
// single-point information (1, 30) or double-point information (3, 31).
//
// These are the only types whose value is a switch position, and so the only
// ones that can chatter — a contact bouncing between open and closed. A
// measurand that reports every time it crosses its deadband is doing its job,
// and calling that chatter would tell the alarm system something false.
func (t TypeID) IsContact() bool {
	switch t {
	case MSpNa1, MSpTb1, MDpNa1, MDpTb1:
		return true
	default:
		return false
	}
}

// Cause is the six-bit cause of transmission: why this ASDU was sent. It is what
// lets a value that arrived by itself be told apart from one that arrived
// because it was asked for, which is exactly what an alarm needs to explain
// itself.
type Cause uint8

const (
	CausePeriodic       Cause = 1 // cyclic data
	CauseBackground     Cause = 2 // background scan
	CauseSpontaneous    Cause = 3 // the value changed by itself
	CauseInitialised    Cause = 4
	CauseRequest        Cause = 5
	CauseActivation     Cause = 6 // a command is being issued
	CauseActCon         Cause = 7 // the station accepted (or rejected, with P/N) the command
	CauseDeactivation   Cause = 8
	CauseDeactCon       Cause = 9
	CauseActTerm        Cause = 10 // the command finished in the field
	CauseRetRem         Cause = 11 // status changed because of a remote command
	CauseRetLoc         Cause = 12 // status changed because of a local operator
	CauseFile           Cause = 13
	CauseIntroGeneral   Cause = 20 // answering a general interrogation
	CauseIntroGroup1    Cause = 21 // groups 1..16 run 21..36
	CauseReqCounter     Cause = 37
	CauseReqCounterGrp1 Cause = 38 // counter groups 1..4 run 38..41

	// The four negative confirmations. A station that does not understand what
	// was asked says so with one of these rather than staying silent, and the
	// difference between them is the difference between "wrong configuration"
	// and "wrong point".
	CauseUnknownType     Cause = 44
	CauseUnknownCause    Cause = 45
	CauseUnknownCommonAd Cause = 46
	CauseUnknownIOA      Cause = 47
)

var causeNames = map[Cause]string{
	CausePeriodic: "periodic", CauseBackground: "background", CauseSpontaneous: "spontaneous",
	CauseInitialised: "initialised", CauseRequest: "request", CauseActivation: "activation",
	CauseActCon: "act-con", CauseDeactivation: "deactivation", CauseDeactCon: "deact-con",
	CauseActTerm: "act-term", CauseRetRem: "return-remote", CauseRetLoc: "return-local",
	CauseFile: "file", CauseIntroGeneral: "interrogated-general", CauseReqCounter: "req-counter",
	CauseUnknownType: "unknown-type", CauseUnknownCause: "unknown-cause",
	CauseUnknownCommonAd: "unknown-common-address", CauseUnknownIOA: "unknown-ioa",
}

func (c Cause) String() string {
	if name, ok := causeNames[c]; ok {
		return name
	}
	switch {
	case c >= CauseIntroGroup1 && c <= 36:
		return "interrogated-group-" + strconv.Itoa(int(c-CauseIntroGroup1)+1)
	case c >= CauseReqCounterGrp1 && c <= 41:
		return "req-counter-group-" + strconv.Itoa(int(c-CauseReqCounterGrp1)+1)
	}
	return "Cause(" + strconv.Itoa(int(c)) + ")"
}

// IsInterrogation reports whether the cause says "this value is part of an
// interrogation response" — general or any of the sixteen groups.
//
// It matters for the event plane: interrogation responses refresh the cache but
// are NOT events. Treating them as events means every reconnect replays the
// whole point list into the event stream.
func (c Cause) IsInterrogation() bool {
	return c >= CauseIntroGeneral && c <= 36
}
