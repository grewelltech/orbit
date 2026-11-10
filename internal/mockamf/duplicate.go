package mockamf

import "github.com/free5gc/ngap/ngapType"

// DuplicateRANNodeAnswer is how the mock answers an NG SETUP REQUEST that
// presents a gNB ID another live association already holds
// (WithDuplicateRANNodeID).
type DuplicateRANNodeAnswer int

const (
	// AcceptDuplicateRANNode answers with NG SETUP RESPONSE and keeps both
	// associations (the default).
	AcceptDuplicateRANNode DuplicateRANNodeAnswer = iota
	// RejectDuplicateRANNode answers with NG SETUP FAILURE, cause Misc
	// unspecified, and keeps the older association.
	RejectDuplicateRANNode
	// ReplaceDuplicateRANNode answers with NG SETUP RESPONSE and ends the
	// older association.
	ReplaceDuplicateRANNode
	// IgnoreDuplicateRANNode sends nothing and keeps both associations.
	IgnoreDuplicateRANNode
	// CrashOnDuplicateRANNode ends the new association, and the mock
	// answers nothing afterwards, as a core that went down would.
	CrashOnDuplicateRANNode
)

// WithDuplicateRANNodeID sets how the mock answers an NG SETUP REQUEST whose
// gNB ID another live association already holds.
func WithDuplicateRANNodeID(answer DuplicateRANNodeAnswer) Option {
	return func(a *AMF) { a.duplicateRANNode = answer }
}

// onRANNode records that p's association presents gNB ID id at NG Setup.
// When another live association already holds id and the mock is not set
// to accept that, it gives the configured answer itself and reports
// answered; the error, if any, ends p's association.
func (a *AMF) onRANNode(p *pipe, id uint32) (answered bool, err error) {
	a.mu.Lock()
	var older []*pipe
	for q, held := range a.ranNodes {
		if q != p && held == id {
			older = append(older, q)
		}
	}
	answer := a.duplicateRANNode
	if len(older) == 0 || answer == AcceptDuplicateRANNode || answer == ReplaceDuplicateRANNode {
		a.ranNodes[p] = id
	}
	if len(older) > 0 && answer == ReplaceDuplicateRANNode {
		for _, q := range older {
			delete(a.ranNodes, q)
		}
	}
	a.mu.Unlock()
	if len(older) == 0 {
		return false, nil
	}
	switch answer {
	case RejectDuplicateRANNode:
		return true, a.send(p, buildNGSetupFailureMisc(ngapType.CauseMiscPresentUnspecified))
	case ReplaceDuplicateRANNode:
		for _, q := range older {
			_ = q.Close()
		}
		return false, nil
	case IgnoreDuplicateRANNode:
		return true, nil
	case CrashOnDuplicateRANNode:
		a.crashed.Store(true)
		return true, errCrashed
	}
	return false, nil
}
