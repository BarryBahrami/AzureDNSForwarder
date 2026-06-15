package sync

import (
	"sync"
	"time"
)

// PeerStatusUpdate is a small record used by the client loop to
// update the in-memory per-peer status without going through the
// config store (which would trigger a save loop).
type PeerStatusUpdate struct {
	LastError  string
	LastAction string
	At         time.Time
}

var (
	statusMu sync.RWMutex
	status   = map[string]PeerStatusUpdate{}
)

// SetPeerStatus records the latest status for a peer. The HTTP
// /api/peers/status handler reads this map to populate the live
// table without persisting it to the YAML.
func SetPeerStatus(name string, u PeerStatusUpdate) {
	statusMu.Lock()
	status[name] = u
	statusMu.Unlock()
}

// GetPeerStatus returns a copy of the latest status, or zero if none.
func GetPeerStatus(name string) PeerStatusUpdate {
	statusMu.RLock()
	defer statusMu.RUnlock()
	return status[name]
}

// StatusSnapshot returns all current peer statuses.
func StatusSnapshot() map[string]PeerStatusUpdate {
	statusMu.RLock()
	defer statusMu.RUnlock()
	out := make(map[string]PeerStatusUpdate, len(status))
	for k, v := range status {
		out[k] = v
	}
	return out
}

// instanceID is the per-process identifier used as the InstanceID
// in Manifest/Envelope. It is set once at startup via SetInstanceID.
var (
	instanceMu sync.RWMutex
	instanceID string
)

// SetInstanceID records the instance id for use by FilterForExport.
func SetInstanceID(s string) {
	instanceMu.Lock()
	instanceID = s
	instanceMu.Unlock()
}

func instanceID_() string {
	instanceMu.RLock()
	defer instanceMu.RUnlock()
	return instanceID
}
