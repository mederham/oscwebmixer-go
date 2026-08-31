// Package oscmsg defines the OSC message shape shared by the desk (UDP) and
// browser (WebSocket) transports.
package oscmsg

// Message mirrors the JSON shape the JS version already used on the wire:
// {"address": "...", "args": [...]}. Args stays untyped since OSC argument
// types (string, int32, float32, ...) map directly onto JSON's number/string/
// bool via encoding/json without any extra type tagging.
type Message struct {
	Address string `json:"address"`
	Args    []any  `json:"args"`
}

// Envelope is the browser-facing WebSocket wire format. Every message sent to
// or received from a mixer/admin client carries an explicit Kind so a status
// push can never be mistaken for real desk data (see the plan's Healthcheck
// section: status information must not be disguised as a fake OSC address).
type Envelope struct {
	Kind    string         `json:"kind"`
	Message *Message       `json:"message,omitempty"`
	Status  *StatusPayload `json:"status,omitempty"`
	Config  *ConfigPayload `json:"config,omitempty"`
}

const (
	KindOSC    = "osc"
	KindStatus = "status"
	KindConfig = "config"
)

// StatusPayload carries desk connectivity/session state, pushed down an
// already-open socket instead of the socket being torn down (see the plan's
// Healthcheck / connection-lifecycle redesign). Locked is independent of
// Desk: an admin-toggled kill switch for personal mixing, not a connectivity
// state, so it rides along on the same push rather than a state of its own.
type StatusPayload struct {
	Desk   string `json:"desk"` // "starting" | "healthy" | "degraded" | "reloading"
	Locked bool   `json:"locked"`
}

// NewOSCEnvelope wraps a Message for transmission to a WS client.
func NewOSCEnvelope(m Message) Envelope {
	return Envelope{Kind: KindOSC, Message: &m}
}

// NewStatusEnvelope wraps a desk status and the personal-mixing lock state
// for transmission to a WS client.
func NewStatusEnvelope(desk string, locked bool) Envelope {
	return Envelope{Kind: KindStatus, Status: &StatusPayload{Desk: desk, Locked: locked}}
}

// NewConfigEnvelope wraps a config payload for transmission to a WS client.
func NewConfigEnvelope(cfg ConfigPayload) Envelope {
	return Envelope{Kind: KindConfig, Config: &cfg}
}
