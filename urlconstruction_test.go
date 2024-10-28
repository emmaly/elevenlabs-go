package elevenlabs

import "testing"

func TestURLConstruction(t *testing.T) {
	c := &Conversation{
		agentID: "test-agent-id",
	}

	// Test direct WebSocket URL
	wsURL := c.getWsURL()
	expected := "wss://api.elevenlabs.io/v1/convai/conversation?agent_id=test-agent-id"
	if wsURL != expected {
		t.Errorf("WebSocket URL incorrect\nexpected: %s\ngot: %s", expected, wsURL)
	}
}
