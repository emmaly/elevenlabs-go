package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gen2brain/malgo"
	"github.com/haguro/elevenlabs-go"
	_ "github.com/joho/godotenv/autoload"
)

type AudioInterface struct {
	inputCallback   func([]byte)
	mutex           sync.Mutex
	isRunning       bool
	context         *malgo.Context
	inputDevice     *malgo.Device
	outputDevice    *malgo.Device
	outputData      []byte
	outputIndex     int
	audioSentCount  int
	audioBuffer     []byte
	samplesPerChunk int

	silenceThreshold float64
	silencePacket    []byte
	lastSendTime     time.Time
	maxSilenceGap    time.Duration
	silenceBuffer    []byte

	debugLog          bool
	debugLogFunctions []string // functions to log debug output; empty means log all
}

func NewAudioInterface() *AudioInterface {
	return &AudioInterface{
		samplesPerChunk:  4000,               // 250ms worth of samples at 16kHz
		silenceThreshold: 120,                // Adjust this value based on testing
		silencePacket:    make([]byte, 8000), // 4000 samples * 2 bytes per sample
		maxSilenceGap:    250 * time.Millisecond,
		lastSendTime:     time.Now(),
	}
}

func (a *AudioInterface) Init() error {
	if a.context != nil {
		return fmt.Errorf("audio interface already initialized")
	}

	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(message string) {
		a.debugLogf("Init", "miniaudio: %s", message)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize context: %w", err)
	}

	// Print available devices
	a.debugLogf("Init", "Audio Device Information:")
	a.debugLogf("Init", "------------------------")

	playbackDevices, err := ctx.Devices(malgo.Playback)
	if err != nil {
		fmt.Printf("Error getting playback devices: %v\n", err)
	} else {
		a.debugLogf("Init", "Playback Devices:")
		for i, device := range playbackDevices {
			a.debugLogf("Init", "\t%d: %s\n", i, device.Name())
		}
	}

	captureDevices, err := ctx.Devices(malgo.Capture)
	if err != nil {
		fmt.Printf("Error getting capture devices: %v\n", err)
	} else {
		a.debugLogf("Init", "Capture Devices:")
		for i, device := range captureDevices {
			a.debugLogf("Init", "\t%d: %s\n", i, device.Name())
		}
	}

	a.debugLogf("Init", "Initializing audio interface...")

	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.context = &ctx.Context

	return nil
}

func (a *AudioInterface) Start(agentOutputAudioFormat string, callback func([]byte)) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if a.context == nil {
		fmt.Println("Audio interface not initialized")
		return
	}

	if a.isRunning {
		fmt.Println("Audio interface already running")
		return
	}

	a.debugLogf("Start", "Starting audio interface...")
	a.inputCallback = callback
	a.isRunning = true
	a.audioSentCount = 0

	// Initialize input device (microphone)
	inputDeviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	inputDeviceConfig.Capture.Channels = 1
	inputDeviceConfig.Alsa.NoMMap = 1
	inputDeviceConfig.Capture.Format = malgo.FormatS16
	inputDeviceConfig.SampleRate = 16000

	a.debugLogf("Start", "Initializing capture device...")
	var err error
	a.inputDevice, err = malgo.InitDevice(*a.context, inputDeviceConfig, malgo.DeviceCallbacks{
		Data: func(outputSamples, inputSamples []byte, framecount uint32) {
			if !a.isRunning {
				return
			}

			// Make a copy of the input samples
			samples := make([]byte, len(inputSamples))
			copy(samples, inputSamples)

			// Calculate audio level
			level := calculateAudioLevel(samples)

			now := time.Now()
			timeSinceLastSend := now.Sub(a.lastSendTime)

			a.mutex.Lock()
			defer a.mutex.Unlock()

			if level > a.silenceThreshold {
				// If we had accumulated silence and have actual audio now,
				// use the silence to complete the current chunk if needed
				if len(a.silenceBuffer) > 0 && len(a.audioBuffer) > 0 {
					remainingForChunk := a.samplesPerChunk*2 - len(a.audioBuffer)
					if remainingForChunk > 0 && len(a.silenceBuffer) >= remainingForChunk {
						// Use just enough silence to complete the chunk
						a.audioBuffer = append(a.audioBuffer, a.silenceBuffer[:remainingForChunk]...)
						a.silenceBuffer = a.silenceBuffer[remainingForChunk:]

						// Send the completed chunk
						chunk := a.audioBuffer[:a.samplesPerChunk*2]
						a.audioBuffer = a.audioBuffer[a.samplesPerChunk*2:]

						a.audioSentCount++
						a.debugLogf("AudioCapture", "Mixed chunk #%d | Level: %.2f | Size: %d bytes",
							a.audioSentCount, level, len(chunk))

						if a.inputCallback != nil {
							a.inputCallback(chunk)
						}
						a.lastSendTime = now
					}
				}

				// Process actual audio
				a.audioBuffer = append(a.audioBuffer, samples...)

				// While we have enough samples for a chunk, send them
				for len(a.audioBuffer) >= a.samplesPerChunk*2 {
					chunk := a.audioBuffer[:a.samplesPerChunk*2]
					a.audioBuffer = a.audioBuffer[a.samplesPerChunk*2:]

					a.audioSentCount++
					a.debugLogf("AudioCapture", "Audio chunk #%d | Level: %.2f | Size: %d bytes",
						a.audioSentCount, level, len(chunk))

					if a.inputCallback != nil {
						a.inputCallback(chunk)
					}
					a.lastSendTime = now
				}
			} else {
				// For silence, only accumulate if we have a partial audio chunk that needs completing
				if len(a.audioBuffer) > 0 {
					a.silenceBuffer = append(a.silenceBuffer, samples...)

					// If we have enough silence to complete a chunk and enough time has passed
					if timeSinceLastSend >= a.maxSilenceGap && len(a.audioBuffer)+len(a.silenceBuffer) >= a.samplesPerChunk*2 {
						// Calculate how much silence we need to complete the chunk
						remainingForChunk := a.samplesPerChunk*2 - len(a.audioBuffer)

						// Combine audio and just enough silence to make a complete chunk
						chunk := make([]byte, a.samplesPerChunk*2)
						copy(chunk, a.audioBuffer)
						copy(chunk[len(a.audioBuffer):], a.silenceBuffer[:remainingForChunk])

						// Update buffers
						a.audioBuffer = nil
						a.silenceBuffer = a.silenceBuffer[remainingForChunk:]

						a.audioSentCount++
						a.debugLogf("AudioCapture", "Completed chunk #%d | Level: %.2f | Size: %d bytes",
							a.audioSentCount, level, len(chunk))

						if a.inputCallback != nil {
							a.inputCallback(chunk)
						}
						a.lastSendTime = now
					}
				} else {
					// No audio buffer to complete, just clear any silence
					a.silenceBuffer = nil
				}
			}
		},
	})

	if err != nil {
		fmt.Printf("Failed to initialize capture device: %v\n", err)
		return
	}

	// Initialize output device (speakers)
	a.debugLogf("Start", "Initializing playback device...")
	outputDeviceConfig := malgo.DefaultDeviceConfig(malgo.Playback)
	outputDeviceConfig.Playback.Channels = 1
	outputDeviceConfig.Alsa.NoMMap = 1

	var outputFormat, sampleRateStr string
	outputFormatParts := strings.Split(agentOutputAudioFormat, "_")
	if len(outputFormatParts) >= 1 {
		outputFormat = outputFormatParts[0]
	}
	if len(outputFormatParts) >= 2 {
		sampleRateStr = outputFormatParts[1]
	}
	// if len(outputFormatParts) >= 3 {} // TODO: implement bit depth

	switch outputFormat {
	case "mp3":
		outputDeviceConfig.Playback.Format = malgo.FormatF32
	case "pcm":
		outputDeviceConfig.Playback.Format = malgo.FormatS16
	case "ulaw":
		outputDeviceConfig.Playback.Format = malgo.FormatU8
	default:
		outputDeviceConfig.Playback.Format = malgo.FormatS16
	}

	outputDeviceConfig.SampleRate = 16000
	if sampleRateStr != "" {
		sampleRate, err := strconv.Atoi(sampleRateStr)
		if err != nil {
			fmt.Printf("Failed to parse sample rate: %v\n", err)
			return
		}
		outputDeviceConfig.SampleRate = uint32(sampleRate)
	}

	a.debugLogf("Start", "Format: %s, Sample Rate: %d", outputFormat, outputDeviceConfig.SampleRate)

	a.outputDevice, err = malgo.InitDevice(*a.context, outputDeviceConfig, malgo.DeviceCallbacks{
		Data: func(outputSamples, inputSamples []byte, framecount uint32) {
			a.mutex.Lock()
			defer a.mutex.Unlock()

			if a.outputData == nil || a.outputIndex >= len(a.outputData) {
				// No data to play, fill with silence
				for i := range outputSamples {
					outputSamples[i] = 0
				}
				return
			}

			// Copy available data to output buffer
			remaining := len(a.outputData) - a.outputIndex
			toCopy := len(outputSamples)
			if remaining < toCopy {
				toCopy = remaining
			}

			copy(outputSamples[:toCopy], a.outputData[a.outputIndex:a.outputIndex+toCopy])
			a.outputIndex += toCopy

			// Fill the rest with silence if needed
			for i := toCopy; i < len(outputSamples); i++ {
				outputSamples[i] = 0
			}

			// Measure audio level
			samples := make([]byte, len(outputSamples))
			copy(samples, outputSamples)
			level := calculateAudioLevel(samples)
			a.debugLogf("AudioReceiveVerbose", "Audio playback | Level: %.2f", level)

			// Debug output less frequently
			if toCopy > 0 && a.outputIndex%1024 == 0 {
				fmt.Printf("Played %d/%d bytes of audio\n", a.outputIndex, len(a.outputData))
			}
		},
	})

	if err != nil {
		fmt.Printf("Failed to initialize playback device: %v\n", err)
		return
	}

	// Start both devices
	a.debugLogf("Start", "Starting capture device...")
	if err := a.inputDevice.Start(); err != nil {
		fmt.Printf("Failed to start capture device: %v\n", err)
		return
	}

	a.debugLogf("Start", "Starting playback device...")
	if err := a.outputDevice.Start(); err != nil {
		fmt.Printf("Failed to start playback device: %v\n", err)
		return
	}

	a.debugLogf("Start", "Audio interface started successfully")
}

func (a *AudioInterface) Stop() {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if a.context == nil {
		fmt.Println("Audio interface not initialized")
		return
	}

	if !a.isRunning {
		fmt.Println("Audio interface already stopped")
		return
	}

	a.debugLogf("Stop", "Stopping audio interface...")
	a.isRunning = false

	// Create a channel to track device stopping
	devicesStopped := make(chan struct{})

	go func() {
		// Stop devices first
		if a.inputDevice != nil {
			a.debugLogf("Stop", "Stopping capture device...")
			_ = a.inputDevice.Stop()
		}

		if a.outputDevice != nil {
			a.debugLogf("Stop", "Stopping playback device...")
			_ = a.outputDevice.Stop()
		}

		close(devicesStopped)
	}()

	// Wait for devices to stop with timeout
	select {
	case <-devicesStopped:
		a.debugLogf("Stop", "Devices stopped successfully")
	case <-time.After(2 * time.Second):
		a.debugLogf("Stop", "Device stop timed out")
	}

	// Uninitialize devices with timeout
	uninitDone := make(chan struct{})
	go func() {
		if a.inputDevice != nil {
			a.debugLogf("Stop", "Uninitializing capture device...")
			a.inputDevice.Uninit()
			a.inputDevice = nil
		}

		if a.outputDevice != nil {
			a.debugLogf("Stop", "Uninitializing playback device...")
			a.outputDevice.Uninit()
			a.outputDevice = nil
		}

		if a.context != nil {
			a.debugLogf("Stop", "Uninitializing audio context...")
			a.context.Uninit()
			a.context = nil
		}

		close(uninitDone)
	}()

	// Wait for uninit with timeout
	select {
	case <-uninitDone:
		a.debugLogf("Stop", "Devices uninitialized successfully")
	case <-time.After(2 * time.Second):
		a.debugLogf("Stop", "Device uninit timed out")
	}

	// Clear any remaining audio data
	a.outputData = nil
	a.outputIndex = 0

	a.debugLogf("Stop", "Audio interface stopped successfully")
}

func (a *AudioInterface) Output(audio []byte) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if a.context == nil {
		fmt.Println("Audio interface not initialized")
		return
	}

	a.debugLogf("Output", "Adding %d bytes to buffer", len(audio))

	// If we're done playing previous audio or this is the first chunk
	if a.outputData == nil || a.outputIndex >= len(a.outputData) {
		a.outputData = make([]byte, len(audio))
		copy(a.outputData, audio)
		a.outputIndex = 0
	} else {
		// Concatenate with existing audio
		remainingAudio := a.outputData[a.outputIndex:]
		newBuffer := make([]byte, len(remainingAudio)+len(audio))
		copy(newBuffer, remainingAudio)
		copy(newBuffer[len(remainingAudio):], audio)
		a.outputData = newBuffer
		a.outputIndex = 0
	}

	a.debugLogf("Output", "Current output buffer: %d bytes, index: %d",
		len(a.outputData), a.outputIndex)
}

func (a *AudioInterface) Interrupt() {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if a.context == nil {
		fmt.Println("Audio interface not initialized")
		return
	}

	a.debugLogf("Interrupt", "Interrupting audio playback")
	// Clear any currently playing audio
	a.outputData = nil
	a.outputIndex = 0
}

func (a *AudioInterface) SetDebug(enabled bool) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.debugLog = enabled
}

func (a *AudioInterface) IsDebug() bool {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.debugLog
}

func (a *AudioInterface) SetDebugFunctions(functions []string) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.debugLogFunctions = functions
}

func (a *AudioInterface) debugLogf(method, format string, args ...interface{}) {
	if a.debugLog && (len(a.debugLogFunctions) == 0 || contains(a.debugLogFunctions, method)) {
		prefix := fmt.Sprintf("[DEBUG:AudioInterface.%s] ", method)
		fmt.Println(strings.TrimSuffix(fmt.Sprintf(prefix+format, args...), "\n"))
	}
}

type Agent struct {
	audioIF           *AudioInterface
	conversation      *elevenlabs.Conversation
	mutex             sync.Mutex
	debugLog          bool
	debugLogFunctions []string
}

func main() {
	agent := &Agent{}
	agent.SetDebug(true)
	agent.SetDebugFunctions([]string{
		"noop",
		"Run",
	})
	agent.Run()
}

func (a *Agent) Run() {
	// Create a context with cancel for the entire application
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup force quit handler (just use SIGQUIT)
	forceQuitChan := make(chan os.Signal, 1)
	signal.Notify(forceQuitChan, syscall.SIGQUIT)
	go func() {
		<-forceQuitChan
		fmt.Println("\nForce quitting...")
		os.Exit(1)
	}()

	// Initialize the ElevenLabs client
	apiKey := os.Getenv("ELEVENLABS_API_KEY")
	if apiKey == "" {
		fmt.Println("Please set ELEVENLABS_API_KEY environment variable")
		return
	}

	client := elevenlabs.NewClient(ctx, apiKey, 30*time.Second)

	// Create audio interface
	a.audioIF = NewAudioInterface()
	a.audioIF.SetDebug(a.debugLog)
	a.audioIF.SetDebugFunctions([]string{
		// "Init",
		"Start",
		// "Stop",
		// "Output",
		// "Interrupt",
		"AudioCapture",
		// "AudioCaptureVerbose",
		"AudioReceive",
		// "AudioReceiveVerbose",
	})
	err := a.audioIF.Init()
	if err != nil {
		fmt.Printf("Failed to create audio interface: %v\n", err)
		return
	}

	// Setup callbacks
	callbacks := elevenlabs.ConversationCallbacks{
		OnAgentResponse: func(response string) {
			a.debugLogf("Run", "ConversationCallbacks.OnAgentResponse: Agent: %s", response)
		},
		OnAgentResponseCorrection: func(original, corrected string) {
			a.debugLogf("Run", "ConversationCallbacks.OnAgentResponseCorrection: Correction:\n  Original: %s\n  Corrected: %s", original, corrected)
		},
		OnUserTranscript: func(transcript string) {
			a.debugLogf("Run", "ConversationCallbacks.OnUserTranscript: You: %s", transcript)
		},
		OnLatencyMeasurement: func(latencyMs int) {
			a.debugLogf("Run", "ConversationCallbacks.OnLatencyMeasurement: Latency: %dms", latencyMs)
		},
	}

	// Create conversation
	agentID := os.Getenv("ELEVENLABS_AGENT_ID")
	if agentID == "" {
		fmt.Println("Please set ELEVENLABS_AGENT_ID environment variable")
		return
	}

	a.debugLogf("Run", "Starting conversation with agent ID: %s", agentID)

	a.conversation = client.NewConversation(agentID, true, a.audioIF, callbacks)
	a.conversation.SetDebug(a.debugLog)
	a.conversation.SetDebugFunctions([]string{
		"StartSession",
		"EndSession",
		"InputCallback",
		"ConversationID",
		"processMessages",
		"handleMessage",
		"handlePing",
		"getWsURL",
		"getSignedURL",
	})

	a.debugLogf("Run", "Initializing conversation session...")
	if err := a.conversation.StartSession(); err != nil {
		a.debugLogf("Run", "Failed to start conversation: %v", err)
		a.debugLogf("Run", "Debug Information:")
		a.debugLogf("Run", "------------------")
		a.debugLogf("Run", "API Key (first/last 4): %s...%s", apiKey[:4], apiKey[len(apiKey)-4:])
		a.debugLogf("Run", "Agent ID: %s", agentID)
		a.debugLogf("Run", "Please check:")
		a.debugLogf("Run", "1. Your API key is correct and has access to Conversation AI")
		a.debugLogf("Run", "2. Your Agent ID is correct")
		a.debugLogf("Run", "3. You have proper network connectivity")
		return
	}

	a.debugLogf("Run", "Conversation started successfully!")
	a.debugLogf("Run", "Audio interface initialized and ready.")
	a.debugLogf("Run", "Press Ctrl+C to end the conversation...")
	a.debugLogf("Run", "Press Ctrl+\\ to force quit if needed...")

	// Regular shutdown handler
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for shutdown signal
	<-sigChan
	a.debugLogf("Run", "Shutting down...")

	// Create a timeout context for the entire shutdown process
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Create a channel to track cleanup completion
	cleanup := make(chan struct{})

	// Start cleanup process in a goroutine
	go func() {
		// Cancel the main context first
		cancel()

		// End the conversation session (which will handle audio and websocket cleanup)
		a.conversation.EndSession()

		// Signal completion
		close(cleanup)
	}()

	// Wait for cleanup or timeout
	select {
	case <-cleanup:
		a.debugLogf("Run", "Application shutdown completed normally")
	case <-shutdownCtx.Done():
		a.debugLogf("Run", "Application shutdown timed out, forcing exit")
	}

	// Final cleanup status
	a.debugLogf("Run", "Shutdown complete")

	// Exit with success status
	os.Exit(0)
}

func (a *Agent) SetDebug(enabled bool) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	if a.debugLog && !enabled || !a.debugLog && enabled {
		if a.audioIF != nil {
			a.audioIF.SetDebug(enabled)
		}
		if a.conversation != nil {
			a.conversation.SetDebug(enabled)
		}
	}
	a.debugLog = enabled
}

func (a *Agent) IsDebug() bool {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.debugLog
}

func (a *Agent) SetDebugFunctions(functions []string) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.debugLogFunctions = functions
}

func (a *Agent) debugLogf(method, format string, args ...interface{}) {
	if a.debugLog && (len(a.debugLogFunctions) == 0 || contains(a.debugLogFunctions, method)) {
		prefix := fmt.Sprintf("[DEBUG:Agent.%s] ", method)
		fmt.Println(strings.TrimSuffix(fmt.Sprintf(prefix+format, args...), "\n"))
	}
}

func calculateAudioLevel(samples []byte) float64 {
	if len(samples) < 2 {
		return 0
	}

	var sum float64
	// Process 16-bit samples
	for i := 0; i < len(samples); i += 2 {
		// Convert two bytes to a 16-bit sample
		sample := int16(samples[i]) | (int16(samples[i+1]) << 8)
		// Get absolute value
		if sample < 0 {
			sample = -sample
		}
		sum += float64(sample)
	}

	// Return average level
	return sum / float64(len(samples)/2)
}

func contains(arr []string, val string) bool {
	for _, a := range arr {
		if a == val {
			return true
		}
	}
	return false
}
