package oscmsg

type Message struct {
	Address string `json:"address"`
	Args    []any  `json:"args"`
}

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

type StatusPayload struct {
	Desk   string `json:"desk"`
	Locked bool   `json:"locked"`
}

func NewOSCEnvelope(m Message) Envelope {
	return Envelope{Kind: KindOSC, Message: &m}
}

func NewStatusEnvelope(desk string, locked bool) Envelope {
	return Envelope{Kind: KindStatus, Status: &StatusPayload{Desk: desk, Locked: locked}}
}

func NewConfigEnvelope(cfg ConfigPayload) Envelope {
	return Envelope{Kind: KindConfig, Config: &cfg}
}
