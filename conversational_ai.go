package elevenlabs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// AudioInterface provides an abstraction for handling audio input and output
type AudioInterface interface {
	// Start begins audio capture and processing. InputCallback should be called
	// with input audio chunks from the user in 16-bit PCM mono format at 16kHz.
	// Recommended chunk size is 4000 samples (250 milliseconds).
	Start(agentOutputAudioFormat string, inputCallback func([]byte))

	// Stop ends all audio processing and cleans up resources
	Stop()

	// Output plays the provided audio bytes (16-bit PCM mono at 16kHz)
	Output(audio []byte)

	// Interrupt stops any current audio playback
	Interrupt()
}

// ConversationCallbacks contains optional callbacks for conversation events
type ConversationCallbacks struct {
	// OnAgentResponse is called when agent responds
	OnAgentResponse func(response string)

	// OnAgentResponseCorrection is called when agent corrects a previous response
	OnAgentResponseCorrection func(original, corrected string)

	// OnUserTranscript is called with user speech transcriptions
	OnUserTranscript func(transcript string)

	// OnLatencyMeasurement is called with latency measurements in milliseconds
	OnLatencyMeasurement func(latencyMs int)
}

// Conversation represents an active conversational AI session
type Conversation struct {
	client                 *Client
	agentID                string
	requiresAuth           bool
	audioIF                AudioInterface
	callbacks              ConversationCallbacks
	wsClient               *wsClient
	conversationID         string
	agentOutputAudioFormat string
	lastInterruptID        int
	shouldStop             chan struct{}
	stopOnce               sync.Once
	mu                     sync.Mutex
	debugLog               bool
	debugLogFunctions      []string
}

// NewConversation creates a new conversation session
func (c *Client) NewConversation(agentID string, requiresAuth bool, audioIF AudioInterface, callbacks ConversationCallbacks) *Conversation {
	return &Conversation{
		client:          c,
		agentID:         agentID,
		requiresAuth:    requiresAuth,
		audioIF:         audioIF,
		callbacks:       callbacks,
		shouldStop:      make(chan struct{}),
		lastInterruptID: -1,
	}
}

// StartSession begins the conversation session
func (c *Conversation) StartSession() error {
	var wsURL string
	var err error

	if c.requiresAuth {
		wsURL, err = c.getSignedURL()
		if err != nil {
			return fmt.Errorf("failed to get websocket URL: %w", err)
		}
	} else {
		wsURL = c.getWsURL()
	}

	c.wsClient = newWebSocketClient(wsURL)
	if err := c.wsClient.connect(); err != nil {
		return fmt.Errorf("failed to connect websocket: %w", err)
	}
	c.wsClient.SetDebug(c.IsDebug())
	c.wsClient.SetDebugFunctions([]string{
		// "connect",
		// "close",
		"send",
		"receive",
	})

	// Start message processing
	go c.processMessages()

	return nil
}

// EndSession stops the conversation session
func (c *Conversation) EndSession() {
	c.stopOnce.Do(func() {
		// Signal stop first
		close(c.shouldStop)

		// Create cleanup channels
		audioStopped := make(chan struct{})
		wsClosed := make(chan struct{})

		// Stop audio interface
		go func() {
			if c.audioIF != nil {
				c.audioIF.Stop()
			}
			close(audioStopped)
		}()

		// Close websocket connection
		go func() {
			if c.wsClient != nil {
				// Try to send close message
				closeMsg := wsMessage{
					Type: "close",
				}
				_ = c.wsClient.send(closeMsg)
				time.Sleep(100 * time.Millisecond)
				c.wsClient.close()
			}
			close(wsClosed)
		}()

		// Wait for both cleanup operations with timeouts
		cleanup := make(chan struct{})
		go func() {
			<-audioStopped
			<-wsClosed
			close(cleanup)
		}()

		// Wait for cleanup or timeout
		select {
		case <-cleanup:
			c.debugLogf("EndSession", "Conversation cleanup completed successfully")
		case <-time.After(5 * time.Second):
			c.debugLogf("EndSession", "Conversation cleanup timed out")
		}
	})
}

// ConversationID returns the ID of the current conversation
func (c *Conversation) ConversationID() string {
	return c.conversationID
}

func (c *Conversation) audioInterfaceStart() {
	// Start audio capture
	c.audioIF.Start(c.agentOutputAudioFormat, func(audio []byte) {
		msg := wsMessage{
			UserAudioChunk: base64.StdEncoding.EncodeToString(audio),
		}
		c.debugLogf("InputCallback", "Sending audio chunk of size: %d bytes", len(audio))
		if err := c.wsClient.send(msg); err != nil {
			// Log error but continue
			c.debugLogf("InputCallback", "Error sending audio: %v", err)
		} else {
			c.debugLogf("InputCallback", "Audio chunk sent successfully")
		}
	})
}

func (c *Conversation) processMessages() {
	for {
		select {
		case <-c.shouldStop:
			return
		default:
			msg, err := c.wsClient.receive()
			if err != nil {
				if err != io.EOF {
					fmt.Printf("Error receiving message: %v", err)
				}
				return
			}

			c.debugLogf("processMessages", "Received message type: %s", msg.Type)

			c.handleMessage(msg)
		}
	}
}

func (c *Conversation) handleMessage(msg wsMessage) {
	switch msg.Type {
	case "internal_vad_score":
		// Ignore internal VAD score messages
		return

	case "internal_turn_probability":
		// Ignore internal turn probability messages
		return

	case "conversation_initiation_metadata":
		if event := msg.ConversationInitMetadata; event != nil {
			c.conversationID = event.SessionID
			c.agentOutputAudioFormat = event.AgentOutputAudioFormat
			c.audioInterfaceStart()
		}

	case "audio":
		if event := msg.AudioEvent; event != nil {
			c.debugLogf("handleMessage", "Received audio event ID: %d", event.EventID)
			c.debugLogf("handleMessage", "Raw audio data length: %d", len(event.AudioData))

			if event.EventID <= c.lastInterruptID {
				c.debugLogf("handleMessage", "Skipping audio event %d (interrupted)", event.EventID)
				return
			}

			audio, err := base64.StdEncoding.DecodeString(event.AudioData)
			if err != nil {
				c.debugLogf("handleMessage", "Error decoding audio: %v", err)
				return
			}

			c.debugLogf("handleMessage", "Decoded %d bytes of audio data", len(audio))
			c.debugLogf("handleMessage", "First few bytes: %v", audio[:min(10, len(audio))])

			c.audioIF.Output(audio)
		} else {
			c.debugLogf("handleMessage", "Received audio message but AudioEvent is nil")
		}

	case "agent_response":
		if c.callbacks.OnAgentResponse != nil {
			if event := msg.AgentResponseEvent; event != nil {
				c.callbacks.OnAgentResponse(event.Text)
			}
		}

	case "agent_response_correction":
		if c.callbacks.OnAgentResponseCorrection != nil {
			if event := msg.AgentResponseCorrectionEvent; event != nil {
				c.callbacks.OnAgentResponseCorrection(
					event.Original,
					event.Corrected,
				)
				c.debugLogf("handleMessage", "Agent response corrected from '%s' to '%s'",
					event.Original, event.Corrected)
			}
		}

	case "user_transcript":
		if c.callbacks.OnUserTranscript != nil {
			if event := msg.UserTranscriptionEvent; event != nil {
				c.callbacks.OnUserTranscript(event.Text)
			}
		}

	case "interruption":
		if event := msg.InterruptionEvent; event != nil {
			c.lastInterruptID = event.ID
			c.audioIF.Interrupt()
		}

	case "ping":
		if event := msg.PingEvent; event != nil {
			c.handlePing(event)
		}

	default:
		c.debugLogf("handleMessage", "Unhandled message type: %s", msg.Type)

	}
}

func (c *Conversation) handlePing(event *pingEvent) {
	response := wsMessage{
		Type:    "pong",
		EventID: event.ID,
	}
	if err := c.wsClient.send(response); err != nil {
		fmt.Printf("Error sending pong: %v\n", err)
		return
	}

	if c.callbacks.OnLatencyMeasurement != nil && event.Latency > 0 {
		c.callbacks.OnLatencyMeasurement(event.Latency)
	}
}

func (c *Conversation) getWsURL() string {
	return fmt.Sprintf("%s/convai/conversation?agent_id=%s", elevenlabsWSBaseURL, url.QueryEscape(c.agentID))
}

func (c *Conversation) getSignedURL() (string, error) {
	b := bytes.Buffer{}
	err := c.client.doRequest(
		c.client.ctx,
		&b,
		http.MethodGet,
		fmt.Sprintf("%s/convai/conversation/get_signed_url?agent_id=%s", c.client.baseURL, url.QueryEscape(c.agentID)),
		&bytes.Buffer{},
		"",
	)
	if err != nil {
		return "", fmt.Errorf("failed to get signed URL: %w", err)
	}

	var result struct {
		URL string `json:"signed_url"`
	}
	if err := json.Unmarshal(b.Bytes(), &result); err != nil {
		return "", fmt.Errorf("failed to parse signed URL response: %w", err)
	}

	if result.URL == "" {
		return "", fmt.Errorf("no URL returned in response")
	}

	return result.URL, nil
}

func (c *Conversation) SetDebug(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.debugLog = enabled
	if c.wsClient != nil {
		c.wsClient.SetDebug(enabled)
	}
}

func (c *Conversation) IsDebug() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.debugLog
}

func (c *Conversation) SetDebugFunctions(functions []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.debugLogFunctions = functions
}

func (c *Conversation) debugLogf(method, format string, args ...interface{}) {
	if c.debugLog && (len(c.debugLogFunctions) == 0 || contains(c.debugLogFunctions, method)) {
		prefix := fmt.Sprintf("[DEBUG:Conversation.%s] ", method)
		fmt.Println(strings.TrimSuffix(fmt.Sprintf(prefix+format, args...), "\n"))
	}
}

// WebSocket client implementation

type wsMessage struct {
	Type                         string                         `json:"type,omitempty"`
	EventID                      int                            `json:"event_id,omitempty"`
	Text                         string                         `json:"text,omitempty"`
	UserAudioChunk               string                         `json:"user_audio_chunk,omitempty"`
	TryTriggerGeneration         bool                           `json:"try_trigger_generation,omitempty"`
	VoiceSettings                *VoiceSettings                 `json:"voice_settings,omitempty"`
	GenerationConfig             *GenerationConfig              `json:"generation_config,omitempty"`
	ConversationInitMetadata     *conversationInitMetadataEvent `json:"conversation_initiation_metadata_event,omitempty"`
	AudioEvent                   *audioEvent                    `json:"audio_event,omitempty"`
	AgentResponseEvent           *agentResponseEvent            `json:"text_event,omitempty"`
	AgentResponseCorrectionEvent *agentResponseCorrectionEvent  `json:"correction_event,omitempty"`
	UserTranscriptionEvent       *userTranscriptionEvent        `json:"transcript_event,omitempty"`
	InterruptionEvent            *interruptionEvent             `json:"interruption_event,omitempty"`
	PingEvent                    *pingEvent                     `json:"ping_event,omitempty"`
}

type GenerationConfig struct {
	ChunkLengthSchedule []int `json:"chunk_length_schedule,omitempty"`
}

type conversationInitMetadataEvent struct {
	SessionID              string `json:"session_id"`
	AgentOutputAudioFormat string `json:"agent_output_audio_format,omitempty"`
}

type audioEvent struct {
	EventID   int    `json:"event_id"`
	AudioData string `json:"audio_base_64"`
}

type agentResponseEvent struct {
	Text string `json:"text"`
}

type agentResponseCorrectionEvent struct {
	Original  string `json:"original"` // Changed field names
	Corrected string `json:"corrected"`
}

type userTranscriptionEvent struct {
	Text string `json:"text"`
}

type interruptionEvent struct {
	ID int `json:"event_id"`
}

type pingEvent struct {
	ID      int `json:"event_id"`
	Latency int `json:"latency"`
}

type wsClient struct {
	conn              *websocket.Conn
	url               string
	mu                sync.Mutex
	debugLog          bool
	debugLogFunctions []string
}

func newWebSocketClient(url string) *wsClient {
	return &wsClient{url: url}
}

func (w *wsClient) connect() error {
	conn, _, err := websocket.DefaultDialer.Dial(w.url, nil)
	if err != nil {
		return err
	}
	w.conn = conn
	return nil
}

func (w *wsClient) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn == nil {
		return nil
	}

	// Create a channel for tracking close completion
	closed := make(chan error)

	go func() {
		// Send close message
		_ = w.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))

		// Allow time for close message to send
		time.Sleep(100 * time.Millisecond)

		// Close the connection
		err := w.conn.Close()
		closed <- err
	}()

	// Wait for close with timeout
	select {
	case err := <-closed:
		w.conn = nil
		return err
	case <-time.After(2 * time.Second):
		w.conn = nil
		return fmt.Errorf("websocket close timed out")
	}
}

func (w *wsClient) send(msg wsMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	doNotLogDebug := false
	if msg.Type == "pong" {
		doNotLogDebug = true
	}

	if !doNotLogDebug {
		w.debugLogf("send", "Sending message type: %s", msg.Type)
		if msg.UserAudioChunk != "" {
			w.debugLogf("send", "Audio chunk length (base64): %d", len(msg.UserAudioChunk))
		}

		// Print raw JSON for debugging
		msgJson, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		w.debugLogf("send", "Raw JSON:\n%s", truncateLongValues(string(msgJson), 100))
	}

	return w.conn.WriteJSON(msg)
}

func (w *wsClient) receive() (wsMessage, error) {
	var msg wsMessage

	// Read the raw message
	_, rawMessage, err := w.conn.ReadMessage()
	if err != nil {
		return msg, err
	}

	// Unmarshal into our struct
	if err := json.Unmarshal(rawMessage, &msg); err != nil {
		fmt.Printf("Error unmarshalling JSON: %v\n", err)
		return msg, err
	}

	doNotLogDebug := false
	if msg.Type == "internal_vad_score" || msg.Type == "internal_turn_probability" || msg.Type == "ping" {
		doNotLogDebug = true
	}

	if !doNotLogDebug {
		// Print raw JSON for debugging
		w.debugLogf("receive", "Raw JSON:\n%s", truncateLongValues(string(rawMessage), 100))
		// Print parsed message for debugging
		w.debugLogf("receive", "Parsed message: %+v", msg)
	}

	return msg, nil
}

func (w *wsClient) SetDebug(enabled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.debugLog = enabled
}

func (w *wsClient) IsDebug() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.debugLog
}

func (w *wsClient) SetDebugFunctions(functions []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.debugLogFunctions = functions
}

func (w *wsClient) debugLogf(method, format string, args ...interface{}) {
	if w.debugLog && (len(w.debugLogFunctions) == 0 || contains(w.debugLogFunctions, method)) {
		prefix := fmt.Sprintf("[DEBUG:wsClient.%s] ", method)
		fmt.Println(strings.TrimSuffix(fmt.Sprintf(prefix+format, args...), "\n"))
	}
}

func truncateLongValues(jsonStr string, maxLen int) string {
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return jsonStr // Return original if can't parse
	}

	// Helper function to truncate strings in the map
	var truncateInMap func(m map[string]interface{})
	truncateInMap = func(m map[string]interface{}) {
		for k, v := range m {
			switch val := v.(type) {
			case string:
				if len(val) > maxLen {
					m[k] = val[:maxLen] + "..." +
						fmt.Sprintf(" [truncated %d chars]", len(val)-maxLen)
				}
			case map[string]interface{}:
				truncateInMap(val)
			}
		}
	}

	truncateInMap(data)

	// Convert back to JSON
	pretty, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return jsonStr // Return original if marshal fails
	}

	return string(pretty)
}

func contains(arr []string, val string) bool {
	for _, a := range arr {
		if a == val {
			return true
		}
	}
	return false
}
