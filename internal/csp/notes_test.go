package csp

import "testing"

func TestNodeUUIDFromNotes(t *testing.T) {
	if got := NodeUUIDFromNotes("sc-csi managed csp-node-uuid=abc-2"); got != "abc-2" {
		t.Errorf("current tag: got %q", got)
	}
	if got := NodeUUIDFromNotes("hand-made server"); got != "" {
		t.Errorf("no tag: got %q", got)
	}
}
