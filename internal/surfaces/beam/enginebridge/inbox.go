package enginebridge

import "encoding/json"

type inboxItem struct {
	ID              string `json:"id"`
	MissionID       string `json:"missionId"`
	AgentName       string `json:"agentName,omitempty"`
	Intent          string `json:"intent,omitempty"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	Reason          string `json:"reason"`
	Report          struct {
		Kind    string `json:"kind"`
		Summary string `json:"summary"`
	} `json:"report"`
}

func (b *Bridge) relayInbox(in <-chan []byte) {
	for {
		select {
		case raw, ok := <-in:
			if !ok {
				return
			}
			if item, decoded := decodeInboxItem(raw); decoded {
				b.emit(item)
			}
		case <-b.done:
			return
		}
	}
}

func decodeInboxItem(raw []byte) (InboxItemAdded, bool) {
	var it inboxItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return InboxItemAdded{}, false
	}
	if it.ID == "" {
		return InboxItemAdded{}, false
	}
	return InboxItemAdded{
		ID:        it.ID,
		MissionID: it.MissionID,
		AgentName: it.AgentName,
		Intent:    it.Intent,
		Reason:    it.Reason,
		Kind:      it.Report.Kind,
		Summary:   it.Report.Summary,
	}, true
}
