package oracle

import (
	"encoding/json"
	"fmt"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A formal-verification oracle (an SMT-backed checker that proves or refutes a
// property of a world's history, or ablates the premises of a proof) speaks
// the ordinary external-oracle protocol: prothesis.oracle_def/v1 to declare
// it, prothesis.oracle_input/v1 on stdin, prothesis.oracle_output/v1 on stdout,
// exit 0/1/2. Its evidence travels in the witness, whose members beyond op_ids
// and key are kept verbatim (schema.Witness.Extra), so no protocol change is
// needed to carry it. FormalWitness is the documented shape of those members,
// so a checker and anything reading its verdict agree on the names.

// Witness member names a formal oracle uses.
const (
	// WitnessUnsatCore names the premises (assertion labels) whose conjunction
	// is unsatisfiable: the minimal reason the property cannot hold.
	WitnessUnsatCore = "unsat_core"
	// WitnessCounterexampleModel is a satisfying assignment that violates the
	// property, as the solver printed it (any JSON value).
	WitnessCounterexampleModel = "counterexample_model"
	// WitnessAblationStatus records the outcome of a premise ablation, e.g.
	// which premise was removed and whether the verdict survived its removal.
	WitnessAblationStatus = "ablation_status"
)

// FormalWitness is the formal-verification evidence a witness can carry.
type FormalWitness struct {
	UnsatCore           []string
	CounterexampleModel json.RawMessage
	AblationStatus      string
}

// IsEmpty reports whether nothing formal is present.
func (f FormalWitness) IsEmpty() bool {
	return len(f.UnsatCore) == 0 && len(f.CounterexampleModel) == 0 && f.AblationStatus == ""
}

// Into writes the formal members into a witness, leaving its other members
// alone.
func (f FormalWitness) Into(w *schema.Witness) error {
	set := func(k string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("oracle: formal witness %s: %w", k, err)
		}
		if w.Extra == nil {
			w.Extra = map[string]json.RawMessage{}
		}
		w.Extra[k] = b
		return nil
	}
	if f.UnsatCore != nil {
		if err := set(WitnessUnsatCore, f.UnsatCore); err != nil {
			return err
		}
	}
	if len(f.CounterexampleModel) > 0 {
		if !json.Valid(f.CounterexampleModel) {
			return fmt.Errorf("oracle: formal witness %s is not JSON", WitnessCounterexampleModel)
		}
		if w.Extra == nil {
			w.Extra = map[string]json.RawMessage{}
		}
		w.Extra[WitnessCounterexampleModel] = append(json.RawMessage(nil), f.CounterexampleModel...)
	}
	if f.AblationStatus != "" {
		if err := set(WitnessAblationStatus, f.AblationStatus); err != nil {
			return err
		}
	}
	return nil
}

// FormalWitnessOf reads the formal members of a witness. ok is false when none
// is present; an error means a member is present but not of its documented
// type, which a reader must treat as malformed evidence, not as absent.
func FormalWitnessOf(w schema.Witness) (f FormalWitness, ok bool, err error) {
	if raw, present := w.Extra[WitnessUnsatCore]; present {
		if err := json.Unmarshal(raw, &f.UnsatCore); err != nil {
			return FormalWitness{}, true, fmt.Errorf("oracle: witness %s must be an array of strings: %w", WitnessUnsatCore, err)
		}
	}
	if raw, present := w.Extra[WitnessCounterexampleModel]; present {
		f.CounterexampleModel = append(json.RawMessage(nil), raw...)
	}
	if raw, present := w.Extra[WitnessAblationStatus]; present {
		if err := json.Unmarshal(raw, &f.AblationStatus); err != nil {
			return FormalWitness{}, true, fmt.Errorf("oracle: witness %s must be a string: %w", WitnessAblationStatus, err)
		}
	}
	return f, !f.IsEmpty(), nil
}
