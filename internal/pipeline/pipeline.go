// Package pipeline is the plugin extension seam described in the plan: one
// choke point per direction, able to mutate a message or veto it outright.
// No implementations exist yet — introduced now, unused, so nothing later
// has to retrofit it. A future Lua-based plugin API becomes an additional
// Interceptor here, with no change to how the pipeline is invoked.
package pipeline

import "github.com/mederham/oscwebmixer-go/internal/oscmsg"

// Interceptor processes one message. Returning ok=false vetoes it — nothing
// further happens with the message. Returning ok=true with a (possibly
// unchanged) Message lets it continue through the pipeline.
type Interceptor interface {
	Handle(msg oscmsg.Message) (out oscmsg.Message, ok bool)
}

// Pipeline is an ordered list of Interceptors, empty by default.
type Pipeline struct {
	interceptors []Interceptor
}

// New builds a Pipeline from zero or more Interceptors, in the order they
// should run.
func New(interceptors ...Interceptor) *Pipeline {
	return &Pipeline{interceptors: interceptors}
}

// Run passes msg through every interceptor in order, short-circuiting with
// ok=false the moment one of them vetoes the message.
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
