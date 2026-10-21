package desk

import (
	"fmt"

	"github.com/mederham/osc"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

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
