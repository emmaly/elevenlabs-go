module agent

go 1.21

require (
	github.com/gen2brain/malgo v0.11.23
	github.com/haguro/elevenlabs-go v0.2.4
	github.com/joho/godotenv v1.5.1
)

require github.com/gorilla/websocket v1.5.3 // indirect

replace github.com/haguro/elevenlabs-go => ../../
