package hub

import (
	"regexp"
	"strconv"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

var snapshotRenameRe = regexp.MustCompile(`^/Snapshots/Rename_Snapshot/([0-9]+)$`)

// handleSnapshotMessage tracks the current snapshot's display name — a Go
// port of the JS version's processSnapshotMsg.
//
// Only the "no snapshot loaded" path (index < 0) has been validated
// against the real desk so far, since that's this desk's actual live state
// right now. The multi-snapshot name-lookup path below (querying
// /Snapshots/names/? and matching the /Snapshots/name replies that come
// back) is ported faithfully from the JS logic but UNTESTED against real
// hardware — revisit once a desk with an actual snapshot loaded is
// available to verify against.
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
	// Synthetic, not a real desk reply — sourced "as the desk" (matching
	// JS's broadcast(msg, config.desk.ip) for this exact case) purely so
	// it doesn't get echoed back to the desk itself; it still reaches
	// every client and external device normally.
	h.broadcast(oscmsg.Message{Address: "/SnapshotName", Args: []any{name}}, deskSource(h.deskIP))
}
