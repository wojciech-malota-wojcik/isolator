package wire

// RunMessage is sent to execute a shell command
type RunMessage struct {

	// Command is a command to execute
	Command string
}

// Ack is sent when command finishes
type Ack struct {
	// Error is the error returned by command
	Error string
}
