package wire

// RunMessage is sent to execute a shell command
type RunMessage struct {

	// Command is a command to execute
	Command string
}
