package hub

import (
	"regexp"
	"strconv"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

var snapshotRenameRe = regexp.MustCompile(`^/Snapshots/Rename_Snapshot/([0-9]+)$`)

func (h *Hub) handleSnapshotMessage(msg oscmsg.Message) {
	switch {
	case msg.Address == "/Snapshots/Current_Snapshot":
		h.currentSnapshot = intArg(msg, 0)
		if h.currentSnapshot < 0 {
			h.setSnapshotName("")
			return
		}
		h.query("/Snapshots/names/?")

	case snapshotRenameRe.MatchString(msg.Address):
		m := snapshotRenameRe.FindStringSubmatch(msg.Address)
		if n, err := strconv.Atoi(m[1]); err == nil && n == h.currentSnapshot && len(msg.Args) > 0 {
			h.setSnapshotName(argAsString(msg.Args[0]))
		}

	case msg.Address == "/Snapshots/name":
		if len(msg.Args) > 0 && argAsInt(msg.Args[0]) == h.currentSnapshot {
			h.setSnapshotName(argAsString(msg.Args[len(msg.Args)-1]))
		}
	}
}

func (h *Hub) setSnapshotName(name string) {
	if h.snapshotName == name {
		return
	}
	h.snapshotName = name
	h.broadcast(oscmsg.Message{Address: "/SnapshotName", Args: []any{name}}, deskSource(h.deskIP))
}
