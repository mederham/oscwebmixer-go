package pipeline

import "github.com/mederham/oscwebmixer-go/internal/oscmsg"

type Interceptor interface {
	Handle(msg oscmsg.Message) (out oscmsg.Message, ok bool)
}

type Pipeline struct {
	interceptors []Interceptor
}

func New(interceptors ...Interceptor) *Pipeline {
	return &Pipeline{interceptors: interceptors}
}

func (p *Pipeline) Run(msg oscmsg.Message) (oscmsg.Message, bool) {
	if p == nil {
		return msg, true
	}
	for _, ic := range p.interceptors {
		var ok bool
		msg, ok = ic.Handle(msg)
		if !ok {
			return oscmsg.Message{}, false
		}
	}
	return msg, true
}
