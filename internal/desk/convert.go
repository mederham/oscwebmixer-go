package desk

import (
	"fmt"

	"github.com/mederham/osc"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// fromOSC converts a github.com/mederham/osc Message (typed Arguments) into
// our transport-agnostic oscmsg.Message (untyped Args), preserving the OSC
// type distinction (int32 vs float32) rather than collapsing everything to
// one Go type.
func fromOSC(m osc.Message) (oscmsg.Message, error) {
	args := make([]any, len(m.Arguments))
	for i, a := range m.Arguments {
		v, err := argToAny(a)
		if err != nil {
			return oscmsg.Message{}, fmt.Errorf("argument %d of %q: %w", i, m.Address, err)
		}
		args[i] = v
	}
	return oscmsg.Message{Address: m.Address, Args: args}, nil
}

func argToAny(a osc.Argument) (any, error) {
	switch a.Typetag() {
	case osc.TypetagInt:
		return a.ReadInt32()
	case osc.TypetagFloat:
		return a.ReadFloat32()
	case osc.TypetagTrue, osc.TypetagFalse:
		return a.ReadBool()
	case osc.TypetagString:
		return a.ReadString()
	case osc.TypetagBlob:
		return a.ReadBlob()
	default:
		return nil, fmt.Errorf("unsupported OSC typetag %q", string(a.Typetag()))
	}
}

// toOSC converts our oscmsg.Message into a github.com/mederham/osc Message
// ready to send to the desk.
//
// NOT YET VALIDATED AGAINST THE REAL DESK: which typetag the desk expects
// for a given address (int32 vs float32, e.g. for send_level/send_pan or
// snapshot indices) is a real protocol question, not something to guess.
// This picks int32 for whole-number Go ints and float32 for everything else
// numeric — flag any address where that guess is wrong once tested against
// hardware, and this mapping will need per-address correction.
func toOSC(m oscmsg.Message) (osc.Message, error) {
	args := make([]osc.Argument, len(m.Args))
	for i, v := range m.Args {
		a, err := anyToArg(v)
		if err != nil {
			return osc.Message{}, fmt.Errorf("argument %d of %q: %w", i, m.Address, err)
		}
		args[i] = a
	}
	return osc.Message{Address: m.Address, Arguments: args}, nil
}

func anyToArg(v any) (osc.Argument, error) {
	switch x := v.(type) {
	case int32:
		return osc.Int(x), nil
	case int:
		return osc.Int(x), nil
	case float32:
		return osc.Float(x), nil
	case float64:
		return osc.Float(x), nil
	case bool:
		return osc.Bool(x), nil
	case string:
		return osc.String(x), nil
	case []byte:
		return osc.Blob(x), nil
	default:
		return nil, fmt.Errorf("unsupported arg type %T", v)
	}
}
